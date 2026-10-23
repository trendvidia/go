// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A stack limit for goroutines inside goplus.AllocDomain.Run: growth past
// it panics with a recoverable error instead of throwing "stack overflow"
// at the process-wide maximum (trendvidia/go#94).

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/stringslite"
	"unsafe"
)

// goplusNoMaxStack is the stack limit of a domain with none.
const goplusNoMaxStack = ^uint64(0)

// goplusRefuseStack reports whether newstack must refuse the growth of
// gp's stack to newsize, and raise goplusMaxStackError on gp instead of
// growing.
//
// It refuses when newsize is past gp's domain's limit, gp is inside a Run
// call on that domain (goplusRefusable, as for the allocation budget),
// and a panic raised here is safe. callerpc is the return address of the
// call whose callee needs the bigger stack: the panic is raised as though
// that call had been to goplusPanicMaxStack (see goplusRaiseMaxStack).
//
// A growth that is past the limit but not safe to refuse is let through,
// by the usual doubling, and the goroutine's next growth that is safe is
// refused instead. The stack doubling then outruns the limit until a safe
// growth catches it. In practice the growth that first crosses the limit
// is almost always safe — it is a call by the recursing guest or library
// code, not by the runtime — so the stack settles within one doubling of
// the limit; see TestMaxStack* for the measured bound.
//
// The safety test is the one the allocation budget uses to refuse an
// allocation (see goplusAllocDomain.refuse), because a refusal is a panic
// at a call in both cases. gp must hold no runtime lock (mp.locks),
// must not be allocating (mp.mallocing), must be preemptible
// (mp.preemptoff) and running (mp.p), and must not be part-way through a
// map write (goplusStackInMapWrite); a panic in any of those leaves
// runtime state a later operation throws on. A runtime lock is covered by
// mp.locks because lock2 raises it; a map write is not a lock, so it is
// walked for separately. callerSP is the stack pointer at callerpc, for
// that walk.
func goplusRefuseStack(gp *g, newsize, callerpc, callerSP uintptr) bool {
	// goplusRaiseMaxStack knows how morestack leaves gp.sched on these
	// architectures only. Elsewhere the limit is never enforced.
	if goarch.ArchFamily != goarch.AMD64 && goarch.ArchFamily != goarch.ARM64 {
		return false
	}
	d := gp.allocDomain
	if d == nil || uint64(newsize) <= d.maxStack.Load() || !goplusRefusable(gp, d) {
		return false
	}
	mp := gp.m
	if mp.locks != 0 || mp.mallocing != 0 || mp.preemptoff != "" || mp.dying != 0 || mp.p == 0 {
		return false
	}
	return !goplusStackInMapWrite(gp, callerpc, callerSP)
}

// goplusStackInMapWrite reports whether gp is part-way through a map write
// at the growth point newstack is handling, whose caller is at callerpc
// with stack pointer callerSP. A map marks itself written for the whole
// of an assignment or delete, its growth and rehash included, and a panic
// there would leave it marked: its next write would throw "concurrent map
// writes". Each allocation a map makes is bounded, so letting these
// growths through cannot run the stack away.
//
// The runtime frames between the growing call and the code that called
// into the map say whether the growth came from a map write. newstack
// runs on the system stack, so the user stack is walked directly.
func goplusStackInMapWrite(gp *g, callerpc, callerSP uintptr) bool {
	var u unwinder
	for u.initAt(callerpc, callerSP, 0, gp, unwindSilentErrors); u.valid(); u.next() {
		for iu, uf := newInlineUnwinder(u.frame.fn, u.symPC()); uf.valid(); uf = iu.next(uf) {
			name := iu.srcFunc(uf).name()
			if stringslite.HasPrefix(name, "runtime.mapassign") || stringslite.HasPrefix(name, "runtime.mapdelete") {
				return true
			}
			if !stringslite.HasPrefix(name, "runtime.") && !stringslite.HasPrefix(name, "internal/runtime/") {
				return false
			}
		}
	}
	return false
}

// goplusRaiseMaxStack makes gp, stopped in morestack and with its stack
// already grown so the panic has room to run, resume in
// goplusPanicMaxStack as though the caller of the function that needed
// more stack had called goplusPanicMaxStack instead. oldsize is the size
// of gp's stack before it grew.
//
// morestack is entered from a function's prologue before the function
// has a frame, so gp.sched already holds its caller's state at the call:
// the return address on top of the stack on amd64, or in the link
// register on arm64, and the caller's frame pointer. Only the PC differs
// from a call to goplusPanicMaxStack, which takes no arguments. The
// unwinder therefore sees an ordinary call, at a call site that has a
// stack map, and the panic runs the caller's deferred calls as a panic in
// any callee would.
//
// The stack is shrunk back once the panic is recovered (see
// goplusRecovered), so that refusals do not leave it ever larger: without
// that, the limit would next be enforced only at the next growth of the
// bigger stack.
func goplusRaiseMaxStack(gp *g, oldsize uintptr) {
	// gp.sched.ctxt, the closure context of the call, is left for gogo to
	// load and clear: goplusPanicMaxStack ignores it, and newstack may not
	// write a pointer with a write barrier.
	gp.sched.pc = abi.FuncPCABIInternal(goplusPanicMaxStack)
	if gp.stackRestore == 0 || oldsize < gp.stackRestore {
		gp.stackRestore = oldsize
	}
}

// goplusRecovered is called by recovery for gp, which has recovered a
// panic. If a stack growth was refused on gp since its stack was last
// restored, it asks gp to stop at its next function call, where newstack
// can shrink the stack it is running on (goplusRestoreStack).
func goplusRecovered(gp *g) {
	if gp.stackRestore != 0 {
		gp.preempt = true
		gp.stackguard0 = stackPreempt
	}
}

// goplusRestoreStack shrinks gp's stack, stopped at a synchronous safe
// point by newstack, to the size it had before its last refused growth,
// or to the smallest size between the two that holds what gp uses. gp
// must be the current user goroutine.
func goplusRestoreStack(gp *g) {
	target := gp.stackRestore
	if target == 0 || !isShrinkStackSafe(gp) || gp.m.libcallsp != 0 {
		return
	}
	gp.stackRestore = 0
	size := gp.stack.hi - gp.stack.lo
	used := gp.stack.hi - gp.sched.sp + stackGuard
	for target < size && used >= target {
		target *= 2
	}
	if target < size {
		copystack(gp, target)
	}
}

// goplusMaxStackError is the panic value of a refused stack growth;
// goplus exports it as ErrMaxStack. It has no fields, so making it an
// interface value does not allocate.
type goplusMaxStackError struct{}

func (goplusMaxStackError) RuntimeError() {}

func (goplusMaxStackError) Error() string {
	return "runtime error: goroutine stack exceeds the goplus.AllocDomain stack limit"
}

// goplusPanicMaxStack raises the panic for a refused stack growth. It is
// entered only from goplusRaiseMaxStack.
//
//go:noinline
func goplusPanicMaxStack() {
	panic(goplusMaxStackError{})
}

//go:linkname goplus_maxStackError goplus.runtime_maxStackError
func goplus_maxStackError() error {
	return goplusMaxStackError{}
}

//go:linkname goplus_setAllocDomainMaxStack goplus.runtime_setAllocDomainMaxStack
func goplus_setAllocDomainMaxStack(d unsafe.Pointer, n uint64) {
	(*goplusAllocDomain)(d).maxStack.Store(n)
}
