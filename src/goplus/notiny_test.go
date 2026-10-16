// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"runtime"
	"strconv"
	"testing"
	"unsafe"
)

var sink []unsafe.Pointer

// allocSmall makes pointer-free allocations of every size under 16 bytes,
// both through new (which the compiler may route to a size-specialised
// malloc) and through paths that allocate inside the runtime: string
// concatenation, []byte to string conversion, and append growth.
//
//go:noinline
func allocSmall(round int) []unsafe.Pointer {
	var ps []unsafe.Pointer
	for n := 1; n < 16; n++ {
		b := make([]byte, n) // runtime makeslice
		b[0] = byte(round)
		ps = append(ps, unsafe.Pointer(unsafe.SliceData(b)))
		s := string(b) // runtime slicebytetostring
		ps = append(ps, unsafe.Pointer(unsafe.StringData(s)))
		g := append([]byte(nil), b...) // runtime growslice
		ps = append(ps, unsafe.Pointer(unsafe.SliceData(g)))
	}
	for _, s := range []string{"a" + strconv.Itoa(round), strconv.Itoa(round) + "bc"} { // concatstrings
		ps = append(ps, unsafe.Pointer(unsafe.StringData(s)))
	}
	ps = append(ps, unsafe.Pointer(new(byte)), unsafe.Pointer(new([3]byte)),
		unsafe.Pointer(new(uint32)), unsafe.Pointer(new(uint64)), unsafe.Pointer(new([15]byte)))
	return heapOnly(ps)
}

// heapOnly drops pointers that are not heap allocations: a 1-byte string
// conversion returns a pointer into the runtime's static table instead of
// allocating.
func heapOnly(ps []unsafe.Pointer) []unsafe.Pointer {
	out := ps[:0]
	for _, p := range ps {
		if _, _, ok := goplus.ObjectExtent(p); ok {
			out = append(out, p)
		}
	}
	return out
}

// ownSlot reports whether p starts its slot, of at most 16 bytes (8 when
// the allocator rounds a small request to the 8-byte class, as it does
// under -race). The first allocation in a shared tiny block also starts
// its slot; TestNoTinyAllocSameP's overlap check is what rules that out.
func ownSlot(t *testing.T, p unsafe.Pointer) bool {
	base, size, ok := goplus.ObjectExtent(p)
	if !ok {
		t.Fatalf("ObjectExtent(%p) not ok", p)
	}
	return base == p && size <= 16
}

// onOneP runs f on the calling goroutine with GOMAXPROCS=1 and the
// goroutine locked to its thread, so every goroutine it starts shares
// one P, and so one mcache and one tiny block.
func onOneP(f func()) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f()
}

func TestNoTinyAllocSameP(t *testing.T) {
	onOneP(func() {
		var flagged, plain []unsafe.Pointer
		done := make(chan []unsafe.Pointer)
		for round := range 50 {
			go func() {
				goplus.SetNoTinyAlloc(true)
				done <- allocSmall(round)
			}()
			flagged = append(flagged, <-done...)
			go func() { done <- allocSmall(round) }()
			plain = append(plain, <-done...)
		}

		for _, p := range flagged {
			if !ownSlot(t, p) {
				base, size, _ := goplus.ObjectExtent(p)
				t.Fatalf("flagged allocation %p is in slot (%p, %d), not a slot of its own", p, base, size)
			}
		}
		// No unflagged allocation lands in a flagged one's slot.
		slots := make(map[uintptr]bool, len(flagged))
		for _, p := range flagged {
			slots[uintptr(p)] = true
		}
		shared := 0
		for _, p := range plain {
			base, _, _ := goplus.ObjectExtent(p)
			if slots[uintptr(base)] {
				t.Fatalf("unflagged allocation %p shares flagged slot %p", p, base)
			}
			if !ownSlot(t, p) {
				shared++
			}
		}
		// A positive marker that the comparison means something: on the
		// same P, unflagged goroutines did use shared tiny blocks.
		if shared == 0 {
			t.Fatalf("no unflagged allocation was in a shared tiny block; the test cannot tell the modes apart")
		}
		// Every round makes at least 50 heap allocations under 16 bytes.
		if len(flagged) < 50*50 || len(plain) < 50*50 {
			t.Fatalf("only %d flagged and %d unflagged heap allocations checked", len(flagged), len(plain))
		}
		t.Logf("%d flagged allocations in own slots; %d of %d unflagged in shared tiny blocks", len(flagged), shared, len(plain))
		sink = append(flagged, plain...)
	})
}

func TestNoTinyAllocInherited(t *testing.T) {
	onOneP(func() {
		done := make(chan []unsafe.Pointer)
		go func() {
			goplus.SetNoTinyAlloc(true)
			go func() { done <- allocSmall(1) }() // inherits
		}()
		for _, p := range <-done {
			if !ownSlot(t, p) {
				t.Fatalf("allocation %p by a goroutine started from a flagged one is not in its own slot", p)
			}
		}
		go func() {
			goplus.SetNoTinyAlloc(true)
			goplus.SetNoTinyAlloc(false)
			go func() { done <- allocSmall(2) }() // inherits off
		}()
		shared := 0
		for _, p := range <-done {
			if !ownSlot(t, p) {
				shared++
			}
		}
		if shared == 0 {
			t.Fatalf("a goroutine started after the setting was cleared still bypasses the tiny allocator")
		}
	})
}

func TestSetNoTinyAllocPrev(t *testing.T) {
	done := make(chan bool)
	go func() {
		a := goplus.SetNoTinyAlloc(true)
		b := goplus.SetNoTinyAlloc(true)
		c := goplus.SetNoTinyAlloc(false)
		d := goplus.SetNoTinyAlloc(false)
		done <- !a && b && c && !d
	}()
	if !<-done {
		t.Fatal("SetNoTinyAlloc did not report the previous setting")
	}
}
