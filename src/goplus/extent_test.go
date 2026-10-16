// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"
	"unsafe"
)

// fullSlice allocates a slice of at least n elements and returns it at its
// full capacity. append on a nil slice rounds the request up to the
// allocator's size (growslice calls roundupsize), so cap*sizeof(T) is the
// slot the allocator handed out, less any malloc header: an expected
// extent derived independently of ObjectExtent's own span arithmetic.
func fullSlice[T any](n int) []T {
	s := append([]T(nil), make([]T, n)...)
	return s[:cap(s)]
}

func checkExtent[T any](t *testing.T, s []T) {
	t.Helper()
	want := unsafe.Pointer(unsafe.SliceData(s))
	wantSize := uintptr(len(s)) * unsafe.Sizeof(s[0])
	for _, i := range []int{0, len(s) / 2, len(s) - 1} {
		p := unsafe.Pointer(&s[i])
		base, size, ok := goplus.ObjectExtent(p)
		if !ok || base != want || size != wantSize {
			t.Fatalf("%T len %d: ObjectExtent(&s[%d]) = (%p, %d, %v), want (%p, %d, true)",
				s, len(s), i, base, size, ok, want, wantSize)
		}
		// The last byte of the object is inside it too.
		last := unsafe.Add(want, wantSize-1)
		if base, size, ok := goplus.ObjectExtent(last); !ok || base != want || size != wantSize {
			t.Fatalf("%T len %d: ObjectExtent(last byte) = (%p, %d, %v), want (%p, %d, true)",
				s, len(s), base, size, ok, want, wantSize)
		}
	}
}

// TestObjectExtentSizes covers every size class, with and without
// pointers (so with heap bits in the span, and with a malloc header), and
// large objects spanning several pages.
func TestObjectExtentSizes(t *testing.T) {
	var keep []any
	checked := 0
	// Pointer-free objects below 16 bytes go to the tiny allocator; they
	// are covered by TestObjectExtentTiny.
	for n := 16; n <= 40<<10; {
		b := fullSlice[byte](n)
		checkExtent(t, b)
		p := fullSlice[unsafe.Pointer]((n + 7) / 8)
		checkExtent(t, p)
		keep = append(keep, b, p)
		checked++
		// Step past the slot just checked, which lands in the next class.
		n = len(b) + 1
	}
	for _, n := range []int{64 << 10, 1<<20 + 3, 9 << 20} {
		checkExtent(t, fullSlice[byte](n))
		checkExtent(t, fullSlice[unsafe.Pointer](n/8))
		checked++
	}
	// A positive marker that the loop walked the size classes rather than
	// stopping early: there are more than 60 classes below 32 KiB.
	if checked < 60 {
		t.Fatalf("checked only %d sizes", checked)
	}
	runtime.KeepAlive(keep)
}

func TestObjectExtentStruct(t *testing.T) {
	type node struct {
		next *node
		val  [5]int64
		name string
	}
	x := new(node)
	base, size, ok := goplus.ObjectExtent(unsafe.Pointer(&x.name))
	if !ok || base != unsafe.Pointer(x) || size < unsafe.Sizeof(*x) {
		t.Fatalf("ObjectExtent(&x.name) = (%p, %d, %v), want (%p, >= %d, true)",
			base, size, ok, x, unsafe.Sizeof(*x))
	}
	runtime.KeepAlive(x)
}

// TestObjectExtentTiny documents that a tiny allocation's extent is the
// 16-byte block it shares with its neighbours.
func TestObjectExtentTiny(t *testing.T) {
	x := new([3]byte)
	base, size, ok := goplus.ObjectExtent(unsafe.Pointer(&x[1]))
	if !ok || size != 16 || uintptr(base) > uintptr(unsafe.Pointer(x)) ||
		uintptr(unsafe.Pointer(x))+3 > uintptr(base)+size {
		t.Fatalf("ObjectExtent(tiny) = (%p, %d, %v), want a 16-byte block containing %p", base, size, ok, x)
	}
	runtime.KeepAlive(x)
}

var global [64]byte

func TestObjectExtentNotHeap(t *testing.T) {
	if _, _, ok := goplus.ObjectExtent(nil); ok {
		t.Errorf("ObjectExtent(nil) ok")
	}
	if _, _, ok := goplus.ObjectExtent(unsafe.Pointer(&global[10])); ok {
		t.Errorf("ObjectExtent(global) ok")
	}
	if ok := stackExtent(); ok {
		t.Errorf("ObjectExtent(stack) ok")
	}
	// stackExtent's buffer must really be on the stack for the check above
	// to mean anything: if it escaped, it would allocate.
	if n := testing.AllocsPerRun(10, func() { stackExtent() }); n != 0 {
		t.Fatalf("stackExtent allocates %v times; its buffer escaped to the heap", n)
	}
}

//go:noinline
func stackExtent() bool {
	var buf [256]byte
	_, _, ok := goplus.ObjectExtent(unsafe.Pointer(&buf[100]))
	return ok
}

// TestObjectExtentConcurrentGC calls ObjectExtent on live objects while
// other goroutines churn the heap and collections run back to back.
func TestObjectExtentConcurrentGC(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sink [][]unsafe.Pointer
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				sink = append(sink, make([]unsafe.Pointer, i%700))
				if len(sink) > 64 {
					sink = sink[:0]
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	iters := 20000
	if testing.Short() {
		iters = 2000
	}
	for i := range iters {
		s := fullSlice[unsafe.Pointer](1 + i%600)
		checkExtent(t, s)
	}
	close(stop)
	wg.Wait()
}

func BenchmarkObjectExtent(b *testing.B) {
	for _, n := range []int{64, 4 << 10, 1 << 20} {
		s := fullSlice[byte](n)
		p := unsafe.Pointer(&s[len(s)/2])
		b.Run(strconv.Itoa(n)+"B", func(b *testing.B) {
			for b.Loop() {
				goplus.ObjectExtent(p)
			}
		})
	}
}
