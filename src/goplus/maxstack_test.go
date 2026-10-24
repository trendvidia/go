// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"errors"
	"fmt"
	"goplus"
	"io"
	"iter"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

// deepFrame is the stack each level of deep uses, roughly: the frame is
// a little smaller, so depths are checked with a margin (slack).
const deepFrame = 1 << 10

// slack is how far past a limit depth*deepFrame may be.
const slack = 4

// deepest is the deepest level deep reached, so a test can tell where the
// recursion stopped.
var deepest int

// deep recurses until limit levels, or forever if limit is negative,
// using about deepFrame bytes of stack per level.
//
//go:noinline
func deep(n, limit int) int {
	var buf [deepFrame - 64]byte
	buf[n%len(buf)] = byte(n)
	deepest = n
	if n == limit {
		return int(buf[0])
	}
	return deep(n+1, limit) + int(buf[(n+1)%len(buf)])
}

func needMaxStack(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("SetMaxStack is not enforced on %s", runtime.GOARCH)
	}
}

// limitProcessStack lowers the process-wide stack maximum for the rest of
// the test, so that a limit that fails to hold ends the test process
// quickly with a fatal "stack overflow" instead of at 1 GB.
func limitProcessStack(t *testing.T, n int) {
	old := debug.SetMaxStack(n)
	t.Cleanup(func() { debug.SetMaxStack(old) })
}

func wantMaxStack(t *testing.T, p any) {
	t.Helper()
	err, ok := p.(error)
	if !ok || !errors.Is(err, goplus.ErrMaxStack) {
		t.Fatalf("Run panicked with %v (%T), want goplus.ErrMaxStack", p, p)
	}
	if _, ok := err.(runtime.Error); !ok {
		t.Fatalf("ErrMaxStack is %T, not a runtime.Error", err)
	}
}

// TestMaxStackRecursion checks that a recursion past the limit inside Run
// panics with ErrMaxStack where the stack reaches the limit, and that the
// panic runs the deferred calls on its way out.
func TestMaxStackRecursion(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	limitProcessStack(t, 16<<20)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)
	deferred := false
	p := runRefused(d, func() {
		defer func() { deferred = true }()
		deep(0, -1)
	})
	wantMaxStack(t, p)
	if !deferred {
		t.Errorf("the deferred call inside Run did not run")
	}
	// The stack doubles, so the recursion stops between half the limit
	// and the limit, and well short of the process-wide maximum.
	if used := deepest * deepFrame; used < limit/2 || used > limit+limit/slack {
		t.Errorf("recursion stopped at %d levels, about %d bytes of stack; want about %d to %d", deepest, used, limit/2, limit)
	}
}

// TestMaxStackErrorsIs and TestMaxStackPrintln are the two programs of
// belluikit/govm#411 that end the process without a limit: errors.Is on a
// deep errors.Join chain, and fmt printing a slice that contains itself,
// which recurses without allocating much.
func TestMaxStackErrorsIs(t *testing.T) {
	needMaxStack(t)
	limitProcessStack(t, 64<<20)
	err := errors.New("leaf")
	for range 1 << 20 {
		err = errors.Join(err)
	}
	d := goplus.NewAllocDomain()
	d.SetMaxStack(1 << 20)
	wantMaxStack(t, runRefused(d, func() { errors.Is(err, io.EOF) }))
}

func TestMaxStackPrintln(t *testing.T) {
	needMaxStack(t)
	limitProcessStack(t, 256<<20)
	s := []any{nil}
	s[0] = s
	d := goplus.NewAllocDomain()
	d.SetMaxStack(64 << 20)
	wantMaxStack(t, runRefused(d, func() { fmt.Fprintln(io.Discard, s) }))
}

// TestMaxStackRepeated refuses many times on one goroutine, inside one
// Run and in new ones. A refusal grows the stack to give the panic room;
// unless the stack is shrunk back once the panic is recovered, the next
// refusal comes only when the bigger stack is full, and the stack doubles
// each time until the process-wide maximum, eight times the limit here,
// ends the process within three refusals.
func TestMaxStackRepeated(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	limitProcessStack(t, 8*limit)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)
	refusals := 0
	refuse := func() {
		defer func() {
			if p := recover(); p != nil {
				wantMaxStack(t, p)
				refusals++
			}
		}()
		deep(0, -1)
	}
	d.Run(func() {
		for range 20 {
			refuse()
		}
	})
	for range 20 {
		d.Run(refuse)
	}
	if refusals != 40 {
		t.Fatalf("%d refusals, want 40", refusals)
	}
	if used := deepest * deepFrame; used > limit+limit/slack {
		t.Errorf("the last recursion reached about %d bytes of stack, past the %d-byte limit", used, limit)
	}
}

// TestMaxStackOutsideRun checks that the limit binds only goroutines
// inside Run on the domain: not the same goroutine after Run returns, not
// a goroutine Run starts, and not a Run on a domain without a limit.
func TestMaxStackOutsideRun(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	const levels = 4 * limit / deepFrame
	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)

	d.Run(func() {})
	deep(0, levels)

	done := make(chan any)
	d.Run(func() {
		go func() {
			defer func() { done <- recover() }()
			deep(0, levels)
		}()
	})
	if p := <-done; p != nil {
		t.Fatalf("goroutine started inside Run panicked: %v", p)
	}

	if p := runRefused(goplus.NewAllocDomain(), func() { deep(0, levels) }); p != nil {
		t.Fatalf("Run on a domain without a limit panicked: %v", p)
	}

	d.SetMaxStack(^uint64(0))
	if p := runRefused(d, func() { deep(0, levels) }); p != nil {
		t.Fatalf("Run after SetMaxStack(MaxUint64) panicked: %v", p)
	}
}

// TestMaxStackPull checks that the body of an iter.Pull iterator, which
// runs on a goroutine of its own, is refused while the goroutine that
// called next is inside Run, and that the panic reaches that caller.
func TestMaxStackPull(t *testing.T) {
	needMaxStack(t)
	limitProcessStack(t, 16<<20)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(1 << 20)
	p := runRefused(d, func() {
		next, stop := iter.Pull(func(yield func(int) bool) {
			deep(0, -1)
			yield(0)
		})
		defer stop()
		next()
	})
	wantMaxStack(t, p)
}

// callbackFrame is the stack each level of the callback recursions below
// uses: more than the code that calls the callback, so each growth comes
// at the callback's own entry, whose caller is sync or the runtime.
const callbackFrame = 16 << 10

var rangeMap sync.Map

// rangeDeep recurses through sync.Map.Range: its callback is called by
// internal/sync.
//
//go:noinline
func rangeDeep(n int) {
	rangeMap.Range(func(k, v any) bool {
		var buf [callbackFrame - 64]byte
		buf[n%len(buf)] = byte(n)
		deepest = n
		rangeDeep(n + 1)
		return buf[(n+1)%len(buf)] == 0
	})
}

// reflectDeep recurses through reflect.Value.Call, whose callee is called
// by the runtime's reflectcall.
//
//go:noinline
func reflectDeep(n int) int {
	var buf [callbackFrame - 64]byte
	buf[n%len(buf)] = byte(n)
	deepest = n
	reflect.ValueOf(reflectDeep).Call([]reflect.Value{reflect.ValueOf(n + 1)})
	return int(buf[(n+1)%len(buf)])
}

// deferDeep recurses through deferred functions that runtime.deferreturn
// calls: a defer in a loop is not open-coded, so the function's return
// runs it from deferreturn.
//
//go:noinline
func deferDeep(n int) {
	for range 1 {
		defer func() {
			var buf [callbackFrame - 64]byte
			buf[n%len(buf)] = byte(n)
			deepest = n
			deferDeep(n + 1)
			callbackSink = buf[(n+1)%len(buf)]
		}()
	}
}

// panicDeep recurses through deferred functions that a panic runs: each
// level panics, and runtime.gopanic calls its deferred function.
//
//go:noinline
func panicDeep(n int) {
	defer func() {
		var buf [callbackFrame - 64]byte
		buf[n%len(buf)] = byte(n)
		deepest = n
		panicDeep(n + 1)
		callbackSink = buf[(n+1)%len(buf)]
	}()
	panic(n)
}

// onces holds the sync.Once of each level of onceDeep, so the test can
// check that each is still usable after the refusal.
var onces []*sync.Once

// onceDeep recurses through sync.Once.Do: its function is called by
// sync.(*Once).doSlow, which holds the Once's mutex meanwhile.
//
//go:noinline
func onceDeep(n int) {
	o := new(sync.Once)
	onces = append(onces, o)
	o.Do(func() {
		var buf [callbackFrame - 64]byte
		buf[n%len(buf)] = byte(n)
		deepest = n
		onceDeep(n + 1)
		callbackSink = buf[(n+1)%len(buf)]
	})
}

// poolDeep recurses through a sync.Pool's New, which sync.(*Pool).Get
// calls.
//
//go:noinline
func poolDeep(n int) {
	p := &sync.Pool{New: func() any {
		var buf [callbackFrame - 64]byte
		buf[n%len(buf)] = byte(n)
		deepest = n
		poolDeep(n + 1)
		return buf[(n+1)%len(buf)]
	}}
	p.Get()
}

var callbackSink byte

// TestMaxStackCallbacks checks recursions whose every growth comes at the
// entry of a function that sync or the runtime called back
// (trendvidia/go#100): before, they were never refused and ran on to the
// process-wide maximum. sync.Map must stay usable after the refusal, and
// no sync.Once may be left locked.
func TestMaxStackCallbacks(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	limitProcessStack(t, 16<<20)
	rangeMap.Store("k", 1)
	for _, tc := range []struct {
		name string
		f    func()
	}{
		{"sync.Map.Range", func() { rangeDeep(0) }},
		{"reflect.Value.Call", func() { reflectDeep(0) }},
		{"deferreturn", func() { deferDeep(0) }},
		{"gopanic", func() { panicDeep(0) }},
		{"sync.Once.Do", func() { onceDeep(0) }},
		{"sync.Pool.New", func() { poolDeep(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deepest = 0
			d := goplus.NewAllocDomain()
			d.SetMaxStack(limit)
			wantMaxStack(t, runRefused(d, tc.f))
			// The stack doubles, so the refusal comes between half the
			// limit and the limit.
			if used := deepest * callbackFrame; used < limit/2 || used > limit {
				t.Errorf("recursion stopped at %d levels, about %d bytes of stack; want about %d to %d", deepest, used, limit/2, limit)
			}
		})
	}
	rangeMap.Store("after", 2)
	n := 0
	rangeMap.Range(func(k, v any) bool { n++; return true })
	if n != 2 {
		t.Errorf("sync.Map has %d entries after the refusal, want 2", n)
	}
	// Each Once's f panicked, the deepest one's at its entry, which Do
	// takes as f returning: Do returns at once on every one, without
	// calling f, and so it must, unless the panic left the Once's mutex
	// locked, when Do hangs; hence the deadline.
	ran := make(chan [2]int, 1)
	go func() {
		returned, called := 0, 0
		for _, o := range onces {
			o.Do(func() { called++ })
			returned++
		}
		ran <- [2]int{returned, called}
	}()
	select {
	case r := <-ran:
		if r[0] != len(onces) || r[0] < 2 || r[1] != 0 {
			t.Errorf("after the refusal, Do returned on %d of %d sync.Onces and called f %d times; want all, and none", r[0], len(onces), r[1])
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Do on a sync.Once still hangs 30s after the refusal: its mutex was left locked")
	}
}
