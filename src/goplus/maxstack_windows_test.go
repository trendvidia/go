// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"sync"
	"syscall"
	"testing"
)

// winCallbackFrame is about the stack each level of newCallbackDeep uses:
// more than the runtime's callback code uses per level, so most growths
// come at newCallbackDeep's entry.
const winCallbackFrame = 16 << 10

var (
	winCallbackOnce sync.Once
	winCallback     uintptr // newCallbackDeep, made callable from C by syscall.NewCallback
)

// newCallbackDeep recurses through a Windows callback: each level calls
// winCallback through its C entry point, as Windows code calls a function
// handed to it, and the runtime's callback code calls newCallbackDeep.
//
//go:noinline
func newCallbackDeep(n uintptr) uintptr {
	var buf [winCallbackFrame - 64]byte
	buf[n%uintptr(len(buf))] = byte(n)
	deepest = int(n)
	syscall.SyscallN(winCallback, n+1)
	return uintptr(buf[(n+1)%uintptr(len(buf))])
}

// TestMaxStackNewCallback checks a stack limit on a recursion through
// syscall.NewCallback callbacks (trendvidia/go#105). The callback is
// ordinary code that the runtime calls back, so a growth at its entry is
// refused there (trendvidia/go#100); the panic must unwind through the
// callback's C frames to Run's caller, and the goroutine must then be
// able to make the call and be refused again. Without the rule for code
// the runtime calls back, every growth here waits for a runtime call that
// never returns, and the recursion runs on to the process-wide maximum.
func TestMaxStackNewCallback(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	limitProcessStack(t, 16<<20)
	winCallbackOnce.Do(func() { winCallback = syscall.NewCallback(newCallbackDeep) })
	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)
	for i := range 2 {
		deepest = 0
		wantMaxStack(t, runRefused(d, func() { syscall.SyscallN(winCallback, 0) }))
		// A growth inside the runtime's callback code is let through, so
		// the stack may double past the limit once before the next growth,
		// at the callback's entry, is refused.
		if used := deepest * winCallbackFrame; used < limit/2 || used > 2*limit {
			t.Errorf("run %d: recursion stopped at %d levels, about %d bytes of stack; want about %d to %d", i, deepest, used, limit/2, 2*limit)
		}
		t.Logf("run %d: refused after %d levels of callbacks", i, deepest)
	}
}
