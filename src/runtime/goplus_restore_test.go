// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"errors"
	"goplus"
	"runtime"
	"runtime/debug"
	"testing"
)

const restoreLimit = 1 << 20

// restorePad calls f about n KiB deeper than its caller.
//
//go:noinline
func restorePad(n int, f func()) {
	var buf [1<<10 - 64]byte
	restoreSink += restoreTouch(buf[:], n)
	if n == 0 {
		f()
		return
	}
	restorePad(n-1, f)
	restoreSink += restoreTouch(buf[:], n+1)
}

// restoreBig uses about 300 KiB of stack, in three frames: the compiler
// moves a local array past 128 KiB to the heap.
//
//go:noinline
func restoreBig(n int) {
	var buf [100 << 10]byte
	restoreSink += restoreTouch(buf[:], n)
	if n%3 != 2 {
		restoreBig(n + 1)
	}
	restoreSink += restoreTouch(buf[:], n+1)
}

// restoreTouch writes and reads b, so that the compiler keeps the array
// behind it on the caller's stack.
//
//go:noinline
func restoreTouch(b []byte, n int) int {
	b[n%len(b)] = byte(n)
	return int(b[(n*7+3)%len(b)])
}

// restoreSmall is a call with a small frame that checks the stack: it is
// not a leaf, which the compiler would leave without the check.
//
//go:noinline
func restoreSmall() {
	var b [16]byte
	restoreSink += restoreTouch(b[:], restoreSink)
}

var (
	restoreSink  int
	restoreDepth int
)

// restoreDeep recurses about 1 KiB a level, without end.
//
//go:noinline
func restoreDeep(n int) {
	var buf [1<<10 - 64]byte
	restoreSink += restoreTouch(buf[:], n)
	restoreDepth = n
	restoreDeep(n + 1)
	restoreSink += restoreTouch(buf[:], n+1)
}

// runRestore runs f inside Run on a domain with restoreLimit on a new
// goroutine, and returns what escaped Run.
func runRestore(t *testing.T, f func()) (escaped any) {
	t.Helper()
	old := debug.SetMaxStack(8 * restoreLimit)
	defer debug.SetMaxStack(old)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(restoreLimit)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { escaped = recover() }()
		d.Run(f)
	}()
	<-done
	return escaped
}

func isMaxStack(p any) bool {
	err, ok := p.(error)
	return ok && errors.Is(err, goplus.ErrMaxStack)
}

// recoverRefusals has five growths refused on a goroutine inside Run,
// each from ordinary code and recovered by a deferred call. atRefusal
// runs just before each refused growth, and inDefer at the start of the
// deferred call, before it grows the stack again within what the refusal
// grew. It fails t if a deferred call recovering a refusal is refused
// again, or if the stack is not back within the limit after a recovery.
func recoverRefusals(t *testing.T, atRefusal, inDefer func()) {
	t.Helper()
	recoverRefusalsOf(t, func() { restoreBig(0) }, atRefusal, inDefer)
}

// recoverRefusalsOf is recoverRefusals with refuse making the growth that
// is refused, about 300 KiB past the frames the refusals fill.
func recoverRefusalsOf(t *testing.T, refuse, atRefusal, inDefer func()) {
	t.Helper()
	var recovered []any
	var sizes []uintptr
	escaped := runRestore(t, func() {
		for range 5 {
			func() {
				defer func() {
					inDefer()
					restoreBig(3) // fits in the stack the refusal grew
					recovered = append(recovered, recover())
				}()
				// About 800 KiB of frames, then 300 KiB more: the growth
				// to 2 MiB is refused.
				restorePad(800, func() {
					atRefusal()
					refuse()
				})
			}()
			restoreSmall() // the restore
			sizes = append(sizes, runtime.GoplusStackSize())
		}
	})
	if escaped != nil {
		t.Fatalf("a deferred call recovering a refusal was refused again: %v escaped Run", escaped)
	}
	if len(recovered) != 5 {
		t.Fatalf("%d refusals recovered, want 5", len(recovered))
	}
	for i, p := range recovered {
		if !isMaxStack(p) {
			t.Errorf("refusal %d recovered %v, want goplus.ErrMaxStack", i, p)
		}
	}
	for i, size := range sizes {
		if size > restoreLimit {
			t.Errorf("after refusal %d was recovered, the stack is %d bytes, past the %d-byte limit (all: %v)", i, size, restoreLimit, sizes)
		}
	}
}

// TestGoplusMaxStackRestoreWaitsForRecovery stops the goroutine at a
// synchronous safe point while a refusal's panic runs its deferred calls,
// on top of the frames that filled the stack. The stack must not be
// restored there (trendvidia/go#103). Shrunk, it leaves the deferred call
// almost no stack, and the call's next growth is refused again, out of
// the call that is recovering the first refusal. Not shrunk, because the
// deferred call uses too much, the restore is spent all the same, and the
// stack stays past the limit for the growths that follow.
func TestGoplusMaxStackRestoreWaitsForRecovery(t *testing.T) {
	needMaxStack(t)
	recoverRefusals(t, func() {}, func() {
		runtime.GoplusRequestStop()
		restoreSmall() // the stop
	})
}

// TestGoplusMaxStackRestoreDisarmedByRefusal arms a restore whose stop
// never came, as a recovery leaves it when the stop it asks for is lost,
// and then has a growth refused. The refusal's panic must not inherit the
// arm: a stop while it runs must not restore the stack under its
// deferred calls.
func TestGoplusMaxStackRestoreDisarmedByRefusal(t *testing.T) {
	needMaxStack(t)
	recoverRefusals(t, runtime.GoplusArmRestore, func() {
		runtime.GoplusRequestStop()
		restoreSmall() // the stop
	})
}

// TestGoplusMaxStackRestoreNestedRecover recovers a second panic inside
// the deferred call that recovers a refusal, as fmt and encoding/json do
// internally. That recovery must not arm the restore: the refusal's panic
// is still running, on top of the frames that filled the stack.
func TestGoplusMaxStackRestoreNestedRecover(t *testing.T) {
	needMaxStack(t)
	recoverRefusals(t, func() {}, func() {
		func() {
			defer func() { recover() }()
			panic("inner")
		}()
		restoreSmall() // where a restore armed by the inner recovery runs
	})
}

// growPastLimit grows the stack past restoreLimit where the growth cannot
// be refused (holding the M).
func growPastLimit() {
	runtime.Acquirem()
	restorePad(1500, restoreSmall) // let through: the M is held
	runtime.Releasem()
}

// TestGoplusMaxStackRestoreCapped grows the stack past the limit where the
// growth cannot be refused, then has growths refused. The stack must come
// back under the limit once a refusal is recovered, or every later
// recursion runs on to the bigger stack's end (trendvidia/go#103).
func TestGoplusMaxStackRestoreCapped(t *testing.T) {
	needMaxStack(t)
	// A collection could shrink the stack the unrefusable growth left.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var depths []int
	escaped := runRestore(t, func() {
		growPastLimit()
		for range 3 {
			func() {
				defer func() {
					if p := recover(); !isMaxStack(p) {
						panic(p)
					}
					depths = append(depths, restoreDepth)
				}()
				restoreDeep(0)
			}()
		}
	})
	if escaped != nil {
		t.Fatalf("Run panicked with %v", escaped)
	}
	if len(depths) != 3 {
		t.Fatalf("%d refusals recovered, want 3", len(depths))
	}
	// The first refusal comes at the end of the stack the unrefusable
	// growth made; the ones after it, at the limit. A level is about 1 KiB,
	// a little less or more by platform, hence the slack.
	if used := depths[0] << 10; used <= restoreLimit+restoreLimit/16 {
		t.Fatalf("the first recursion reached about %d bytes of stack, within the %d-byte limit: the growth past it was not let through", used, restoreLimit)
	}
	for i, n := range depths[1:] {
		if used := n << 10; used > restoreLimit+restoreLimit/16 {
			t.Errorf("recursion %d reached about %d bytes of stack, past the %d-byte limit (all: %v)", i+2, used, restoreLimit, depths)
		}
	}
}

// TestGoplusMaxStackRestoreCappedOutsideRun is TestGoplusMaxStackRestoreCapped
// with the refusal recovered outside Run, where the goroutine has no
// domain: the stack must still come back under the limit of the domain
// that refused, or the goroutine's next Run recurses to the bigger
// stack's end.
func TestGoplusMaxStackRestoreCappedOutsideRun(t *testing.T) {
	needMaxStack(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	old := debug.SetMaxStack(8 * restoreLimit)
	defer debug.SetMaxStack(old)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(restoreLimit)
	var escaped any
	var first int
	var size uintptr
	done := make(chan struct{})
	go func() {
		defer close(done)
		func() {
			defer func() { escaped = recover() }()
			d.Run(func() {
				growPastLimit()
				restoreDeep(0)
			})
		}()
		first = restoreDepth
		restoreSmall() // the restore
		size = runtime.GoplusStackSize()
	}()
	<-done
	if !isMaxStack(escaped) {
		t.Fatalf("Run panicked with %v, want goplus.ErrMaxStack", escaped)
	}
	if used := first << 10; used <= restoreLimit+restoreLimit/16 {
		t.Fatalf("the recursion reached about %d bytes of stack, within the %d-byte limit: the growth past it was not let through", used, restoreLimit)
	}
	if size > restoreLimit {
		t.Errorf("after the refusal was recovered outside Run, the stack is %d bytes, past the %d-byte limit", size, restoreLimit)
	}
}

// hookedRestoreHelper stands for runtime code that compiled code calls, as
// hookedHelper does: the late tests below have the stack limit treat the
// functions whose names start with hookedPrefix so. Its growths come in
// hookedRestoreLeaf, called by it, so they are refused late, as it
// returns to its caller (trendvidia/go#97).
//
//go:noinline
func hookedRestoreHelper() { hookedRestoreLeaf(0) }

// hookedRestoreLeaf uses about 300 KiB of stack, in three frames, as
// restoreBig does.
//
//go:noinline
func hookedRestoreLeaf(n int) {
	var buf [100 << 10]byte
	restoreSink += restoreTouch(buf[:], n)
	if n%3 != 2 {
		hookedRestoreLeaf(n + 1)
	}
	restoreSink += restoreTouch(buf[:], n+1)
}

// recoverLateRefusals is recoverRefusals with each growth refused late,
// as hookedRestoreHelper returns. It fails t unless every refusal was.
func recoverLateRefusals(t *testing.T, atRefusal, inDefer func()) {
	t.Helper()
	runtime.SetGoplusTestRuntimeFunc(hookedPrefix)
	defer runtime.SetGoplusTestRuntimeFunc("")
	before := runtime.GoplusLateRefusals()
	recoverRefusalsOf(t, hookedRestoreHelper, atRefusal, inDefer)
	if late := runtime.GoplusLateRefusals() - before; late < 5 {
		t.Fatalf("%d of the 5 refusals waited for a runtime call; the test does not test late refusals", late)
	}
}

// TestGoplusMaxStackRestoreLateWaitsForRecovery is
// TestGoplusMaxStackRestoreWaitsForRecovery with the refusals raised late.
func TestGoplusMaxStackRestoreLateWaitsForRecovery(t *testing.T) {
	needMaxStack(t)
	recoverLateRefusals(t, func() {}, func() {
		runtime.GoplusRequestStop()
		restoreSmall() // the stop
	})
}

// TestGoplusMaxStackRestoreLateDisarmedByRefusal is
// TestGoplusMaxStackRestoreDisarmedByRefusal with the refusal raised late:
// a late refusal must disarm a restore left armed, as one in place does.
func TestGoplusMaxStackRestoreLateDisarmedByRefusal(t *testing.T) {
	needMaxStack(t)
	recoverLateRefusals(t, runtime.GoplusArmRestore, func() {
		runtime.GoplusRequestStop()
		restoreSmall() // the stop
	})
}

// TestGoplusMaxStackRestoreLateNestedRecover is
// TestGoplusMaxStackRestoreNestedRecover with the refusal raised late.
func TestGoplusMaxStackRestoreLateNestedRecover(t *testing.T) {
	needMaxStack(t)
	recoverLateRefusals(t, func() {}, func() {
		func() {
			defer func() { recover() }()
			panic("inner")
		}()
		restoreSmall() // where a restore armed by the inner recovery runs
	})
}

// TestGoplusMaxStackRestoreLateCapped is TestGoplusMaxStackRestoreCapped
// with the refusals raised late: the size a late refusal records to shrink
// back to must be capped at the limit too.
func TestGoplusMaxStackRestoreLateCapped(t *testing.T) {
	needMaxStack(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.SetGoplusTestRuntimeFunc(hookedPrefix)
	defer runtime.SetGoplusTestRuntimeFunc("")
	before := runtime.GoplusLateRefusals()
	var depths []int
	escaped := runRestore(t, func() {
		growPastLimit()
		for range 3 {
			func() {
				defer func() {
					if p := recover(); !isMaxStack(p) {
						panic(p)
					}
					depths = append(depths, lateLevels)
				}()
				lateRecurse(0)
			}()
		}
	})
	if escaped != nil {
		t.Fatalf("Run panicked with %v", escaped)
	}
	if late := runtime.GoplusLateRefusals() - before; late < 3 {
		t.Fatalf("%d of the 3 refusals waited for a runtime call; the test does not test late refusals", late)
	}
	if len(depths) != 3 {
		t.Fatalf("%d refusals recovered, want 3", len(depths))
	}
	// A level of lateRecurse is about lateFrame, and its growths come in
	// hookedLeaf, 16 KiB deeper, hence the slack.
	if used := depths[0] * lateFrame; used <= restoreLimit {
		t.Fatalf("the first recursion reached about %d bytes of stack, within the %d-byte limit: the growth past it was not let through", used, restoreLimit)
	}
	for i, n := range depths[1:] {
		if used := n * lateFrame; used > restoreLimit {
			t.Errorf("recursion %d reached about %d bytes of stack, past the %d-byte limit (all: %v)", i+2, used, restoreLimit, depths)
		}
	}
}

// TestGoplusMaxStackRestoreStopDropped has the stop a recovery asks for
// arrive while the M cannot be preempted: the deferred call that recovers
// the refusal still holds m.locks when recovery resumes the goroutine.
// newstack lets such a stop go and resumes the goroutine, as it does for a
// preemption request. The restore must still happen, at a later stop,
// rather than be lost until the next refusal: lost at each of three
// refusals in a row, the stack doubles to the process-wide maximum.
func TestGoplusMaxStackRestoreStopDropped(t *testing.T) {
	needMaxStack(t)
	var sizes []uintptr
	escaped := runRestore(t, func() {
		for range 3 {
			func() {
				defer func() {
					recover()
					runtime.Acquirem() // held when recovery resumes the goroutine
				}()
				restorePad(800, func() { restoreBig(0) })
			}()
			restoreSmall() // the stop, at which the M cannot be preempted
			runtime.Releasem()
			restoreSmall()
			restoreSmall()
			sizes = append(sizes, runtime.GoplusStackSize())
		}
	})
	if escaped != nil {
		t.Fatalf("Run panicked with %v", escaped)
	}
	for i, size := range sizes {
		if size > restoreLimit {
			t.Errorf("after refusal %d was recovered with the stop dropped, the stack is %d bytes, past the %d-byte limit (all: %v)", i, size, restoreLimit, sizes)
		}
	}
}
