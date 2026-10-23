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
// A panic here is safe when the goroutine can take one at all
// (goplusCanPanic, the test the allocation budget also applies) and the
// call is made by code that expects a callee to panic (goplusPanicSafeAt).
// A growth past the limit that is not safe to refuse is let through, by
// the usual doubling, and the goroutine's next safe growth is refused
// instead.
func goplusRefuseStack(gp *g, newsize, callerpc uintptr) bool {
	// goplusRaiseMaxStack knows how morestack leaves gp.sched on these
	// architectures only. Elsewhere the limit is never enforced.
	if goarch.ArchFamily != goarch.AMD64 && goarch.ArchFamily != goarch.ARM64 {
		return false
	}
	d := gp.allocDomain
	if d == nil || uint64(newsize) <= d.maxStack.Load() || !goplusRefusable(gp, d) {
		return false
	}
	if !goplusCanPanic(gp, 0) || gp.m.p == 0 {
		return false
	}
	return goplusPanicSafeAt(callerpc)
}

// goplusCanPanic reports whether gp, the goroutine on whose behalf the
// runtime is allocating or growing a stack, can take a panic there: it
// is its M's current user goroutine, it holds no runtime lock beyond the
// held its caller took itself (lock2 counts each in m.locks), and it is
// not allocating, unpreemptible or dying. Both refusals, of an allocation
// past a domain's budget and of a stack growth past its limit, need it.
func goplusCanPanic(gp *g, held int32) bool {
	mp := gp.m
	return gp == mp.curg && mp.locks == held && mp.mallocing == 0 && mp.preemptoff == "" && mp.dying == 0
}

// goplusPanicSafeAt reports whether a panic may be raised at the call
// whose return address is pc, in place of the call.
//
// Go code expects any call to panic, but the runtime and the packages
// that share its invariants do not: a panic in the middle of one of them
// can leave a map marked as being written, a mutex with a waiter that is
// never woken, or the panic machinery itself inconsistent, and each of
// those later hangs or throws, which is what the limit exists to prevent.
// So the call must not be made by such code, including code inlined at
// pc. Map code marks a map written only while its own frames, or the
// hash and equality functions it calls, are the callers, so this also
// keeps a refusal out of a map write. Calls from a function the runtime
// cannot find, such as generated code that is not registered, are not
// refused either: the panic could not unwind through it.
func goplusPanicSafeAt(pc uintptr) bool {
	f := findfunc(pc - 1)
	if !f.valid() {
		return false
	}
	for iu, uf := newInlineUnwinder(f, pc-1); uf.valid(); uf = iu.next(uf) {
		if goplusRuntimeOwned(iu.srcFunc(uf).name()) {
			return false
		}
	}
	return true
}

// goplusRuntimeOwned reports whether the function with symbol name name
// belongs to the runtime or to a package whose state a panic at one of
// its calls could leave inconsistent in a way that later hangs or throws.
//
// The runtime implements functions of other packages under their names,
// by linkname, and names them pkg.runtime_X by convention
// (sync.runtime_Semacquire, internal/poll.runtime_pollWait); those are
// runtime code too.
//
// The checkptr functions are the exception. The compiler calls them in
// every function it instruments for the race detector or -d=checkptr, so
// a recursion's growth comes in one of them at every level; they only
// read, and hold no state a panic could leave half-changed. Counting them
// as runtime code would let every growth of such a recursion through, and
// the stack would double past the limit to the process-wide maximum.
func goplusRuntimeOwned(name string) bool {
	if stringslite.HasPrefix(name, "runtime.checkptr") {
		return false
	}
	for _, prefix := range [...]string{
		"runtime.",
		"internal/runtime/",
		"sync.",
		"internal/sync.",
		// Hash and equality functions the compiler generates for types,
		// which maps call while they are marked as being written.
		"type:",
	} {
		if stringslite.HasPrefix(name, prefix) {
			return true
		}
	}
	// The last path element starts the package name; the function's own
	// name follows the first dot after it.
	rest := name
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			rest = name[i+1:]
			break
		}
	}
	if dot := stringslite.IndexByte(rest, '.'); dot >= 0 {
		return stringslite.HasPrefix(rest[dot+1:], "runtime_")
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
// can shrink the stack it is running on (goplusRestoreStack). It raises
// the stack guard without setting gp.preempt, so that newstack resumes gp
// after the shrink instead of descheduling it.
func goplusRecovered(gp *g) {
	if gp.stackRestore != 0 {
		gp.stackguard0 = stackPreempt
	}
}

// goplusRestoreStack shrinks gp's stack, stopped at a synchronous safe
// point by newstack, to the size it had before its last refused growth,
// or to the smallest size between the two that holds what gp uses and the
// frame of the function it is entering. gp must be the current user
// goroutine. It reports whether a restore was pending, done or not.
func goplusRestoreStack(gp *g) bool {
	target := gp.stackRestore
	if target == 0 {
		return false
	}
	if !isShrinkStackSafe(gp) || gp.m.libcallsp != 0 {
		return true
	}
	gp.stackRestore = 0
	size := gp.stack.hi - gp.stack.lo
	used := gp.stack.hi - gp.sched.sp + stackGuard
	if f := findfunc(gp.sched.pc); f.valid() {
		used += uintptr(funcMaxSPDelta(f))
	}
	for target < size && used >= target {
		target *= 2
	}
	if target < size {
		copystack(gp, target)
	}
	return true
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
