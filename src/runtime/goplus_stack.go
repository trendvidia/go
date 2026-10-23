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
	"internal/runtime/atomic"
	"internal/stringslite"
	"unsafe"
)

// goplusNoMaxStack is the stack limit of a domain with none.
const goplusNoMaxStack = ^uint64(0)

// goplusRefuseStack reports whether newstack must refuse the growth of
// gp's stack to newsize, and how: refuse is to raise goplusMaxStackError
// on gp in place of the call that needs the stack (goplusRaiseMaxStack),
// late is to raise it once the runtime code that made that call returns
// to the code that called it (goplusRaiseMaxStackLate).
//
// It refuses when newsize is past gp's domain's limit and gp is inside a
// Run call on that domain (goplusRefusable, as for the allocation budget).
// callerpc is the return address of the call whose callee needs the
// bigger stack. If the code that made that call expects a callee to panic
// (goplusPanicSafeAt), or the callee is ordinary code that the runtime or
// sync calls back (goplusOrdinaryCallee), and the goroutine can take a
// panic at all
// (goplusCanPanic, the test the allocation budget also applies), the
// panic is raised as though that call had been to goplusPanicMaxStack.
// Otherwise the call was made by the runtime, whose frames a panic must
// not abandon, and the refusal waits for the runtime to return. Whether
// the goroutine can take the panic is asked again then, by
// goplusHijackReturn, and not here: runtime code that holds a lock while
// it grows, as getitab does under itabLock, has released it by the time
// it returns. A growth past the limit that neither can refuse is let
// through, by the usual doubling, and the goroutine's next growth is
// considered again.
func goplusRefuseStack(gp *g, newsize, callerpc uintptr) (refuse, late bool) {
	// goplusRaiseMaxStack knows how morestack leaves gp.sched, and
	// goplusHijackReturn is written, on these architectures only.
	// Elsewhere the limit is never enforced.
	if goarch.ArchFamily != goarch.AMD64 && goarch.ArchFamily != goarch.ARM64 {
		return false, false
	}
	d := gp.allocDomain
	if d == nil || uint64(newsize) <= d.maxStack.Load() || !goplusRefusable(gp, d) {
		return false, false
	}
	if goplusPanicSafeAt(callerpc) || goplusOrdinaryCallee(gp.sched.pc, callerpc) {
		return goplusCanPanic(gp, 0) && gp.m.p != 0, false
	}
	return false, true
}

// goplusOrdinaryCallee reports whether a panic may be raised in place of
// a call from runtime or sync code because of what is called: the function
// containing pc, which needs more stack, is ordinary code, and the caller,
// whose return address is callerpc, is a function the runtime can unwind.
//
// Runtime and sync code that calls ordinary code calls a function it was
// given: a deferred call, a function reflect calls, a cgo callback, the
// function of a sync.Once, a sync.Map.Range callback, a sync.Pool's New.
// That code may panic anyway, so its caller already survives a panic out
// of the call, and a refusal there is that panic, before the function's
// first statement. Where the runtime calls ordinary code that must not
// panic, it holds a runtime lock (time's sendTime under the timer's
// sendLock), which goplusCanPanic checks. Without this, a recursion
// through such a callback grows at the callback's own entry at every level,
// whose caller is runtime code, and is never refused (trendvidia/go#100).
//
// Not ordinary: the runtime's own code (goplusRuntimeOwned), and the std
// packages the runtime and sync call directly, trusting them not to panic
// (internal/..., sync/atomic).
func goplusOrdinaryCallee(pc, callerpc uintptr) bool {
	f := findfunc(pc)
	if !f.valid() || !findfunc(callerpc-1).valid() {
		return false
	}
	return goplusOrdinaryName(funcname(f))
}

// goplusOrdinaryName reports whether the function with symbol name name is
// ordinary code for goplusOrdinaryCallee.
func goplusOrdinaryName(name string) bool {
	if goplusTestRuntimeFunc != "" && stringslite.HasPrefix(name, goplusTestRuntimeFunc) {
		return false
	}
	return name != "" && !goplusRuntimeOwned(name) &&
		!stringslite.HasPrefix(name, "internal/") &&
		!stringslite.HasPrefix(name, "sync/atomic.")
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
func goplusRuntimeOwned(name string) bool {
	if goplusIsTestRuntimeFunc(name) {
		return true
	}
	return goplusRuntimeCode(name)
}

// goplusRuntimeCode reports whether the function with symbol name name is
// runtime code as goplusRuntimeOwned counts it, without the test hook.
func goplusRuntimeCode(name string) bool {
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

// goplusHijack is a return address on a goroutine's stack that
// goplusRaiseMaxStackLate overwrote, so that the function it belongs to
// returns into goplusHijackReturn instead of to its caller.
type goplusHijack struct {
	slot uintptr // where on the stack the return address is; 0 if none
	pc   uintptr // the return address, into the caller
	ret  uintptr // what slot holds instead: the PC of goplusHijackReturn
}

// goplusRaiseMaxStackLate refuses a growth of gp's stack, stopped in
// morestack and already grown, whose function was called by runtime code:
// gp is to panic with goplusMaxStackError once the runtime returns to the
// code that called it, as though that call had been to
// goplusPanicMaxStack. oldsize is the size of gp's stack before it grew.
//
// A panic cannot be raised in the runtime frames, which may hold a map,
// a lock or the panic machinery part-way changed. So they are left to
// complete: the walk up gp's stack passes through them to the first call
// made by code that expects a callee to panic (goplusPanicSafeAt), and
// the return address of that call is overwritten with goplusHijackReturn,
// which raises the panic there. The unwinder reads the return address it
// stood for while it is pending (goplusReturnAddress), so the stack walks
// of the garbage collector, stack copies, tracebacks and profiles see the
// frames as they are.
//
// The runtime call completes before the panic, unlike a refusal in its
// place, so the refusal waits only for functions that compiled code calls
// to carry out an operation of the language (goplusLateRefusable). Their
// effects, a value received from a channel or a key added to a map, are
// complete before they return. Runtime API that other packages call by
// name is left out: a call such as runtime.LockOSThread, or one by sync to
// runtime_SemacquireMutex, acquires something its caller releases in a
// deferred call that the call returning would precede, and a panic
// between the two would keep it; and a call by iter.Pull to newcoro makes
// a coroutine that only the functions iter.Pull has yet to return can end.
//
// When the walk finds no such call, the growth is let through and gp's
// next growth is considered again. That includes a growth while a
// refusal is already pending on gp: the runtime frames below it are
// running code that called back into ordinary code, such as a deferred
// call or a reflect call, whose own growths are refused in place.
func goplusRaiseMaxStackLate(gp *g, oldsize uintptr) {
	if gp.goplusHijack.slot != 0 {
		return
	}
	var u unwinder
	u.initAt(^uintptr(0), ^uintptr(0), ^uintptr(0), gp, unwindSilentErrors)
	// The first frame is the function that needs the stack, called by the
	// runtime: goplusPanicSafeAt said so of its return address.
	for u.next(); u.valid(); u.next() {
		frame := &u.frame
		// A runtime frame, which is to complete. One the unwinder treats
		// specially (gopanic, systemstack, mcall) ends the walk, and so
		// does a frame of code that is not the runtime's, such as a
		// function a runtime function was inlined into at its call.
		// Wrappers are ordinary here: generated code, and deferreturn,
		// which the toolchain marks as one.
		if id := frame.fn.funcID; id != abi.FuncIDNormal && id != abi.FuncIDWrapper || frame.lr == 0 {
			return
		}
		name := funcname(frame.fn)
		if !goplusRuntimeOwned(name) {
			return
		}
		if !goplusPanicSafeAt(frame.lr) {
			continue
		}
		if !goplusLateRefusable(name) {
			return
		}
		// Where the unwinder found frame.lr (see resolveInternal).
		slot := frame.fp - goarch.PtrSize
		if usesLR {
			slot = frame.sp
		}
		if *(*uintptr)(unsafe.Pointer(slot)) != frame.lr {
			return
		}
		ret := goplusHijackReturnPC()
		gp.goplusHijack = goplusHijack{slot: slot, pc: frame.lr, ret: ret}
		*(*uintptr)(unsafe.Pointer(slot)) = ret
		goplusLateRefusals.Add(1)
		if gp.stackRestore == 0 || oldsize < gp.stackRestore {
			gp.stackRestore = oldsize
		}
		return
	}
}

// goplusLateRefusable reports whether a refusal of a stack growth may wait
// for the runtime function with symbol name name to return to its caller,
// and be raised there (see goplusRaiseMaxStackLate). Those are the
// functions compiled code calls to carry out an operation of the language:
// runtime functions with unexported names that are neither methods nor
// closures, such as runtime.assertE2I2, runtime.mapassign_faststr or
// runtime.deferproc. Exported runtime functions, methods, and functions
// the runtime implements for other packages are their API, which the
// code calling them may pair with a release; so are the unexported ones
// other packages of the standard library call by name
// (goplusLinknamedStd).
func goplusLateRefusable(name string) bool {
	if goplusIsTestRuntimeFunc(name) {
		return true
	}
	rest, ok := stringslite.CutPrefix(name, "runtime.")
	if !ok || rest == "" {
		return false
	}
	if c := rest[0]; !('a' <= c && c <= 'z' || c == '_') {
		return false
	}
	for _, std := range goplusLinknamedStd {
		if rest == std {
			return false
		}
	}
	return stringslite.IndexByte(rest, '.') < 0
}

// goplusLinknamedStd are the unexported runtime functions that other
// packages of the standard library call by name, which //go:linknamestd
// allows. Their callers are not compiled code carrying out an operation
// of the language but code that uses what they return, and a refusal that
// waited for one would come before that code could: iter.Pull would lose
// the coroutine newcoro had made for it, parked forever.
// TestGoplusLinknamedStd keeps the list.
var goplusLinknamedStd = [...]string{
	"bootstrapRand",
	"coroswitch",
	"goplusCoroRecovers",
	"newcoro",
	"reflectcall",
	"spillArgs",
	"unspillArgs",
}

// goplusReturnAddress returns pc, a return address read from gp's stack,
// or if goplusRaiseMaxStackLate overwrote it, the return address it stood
// for. Each runtime function that reads its own return address after a
// call that can grow the stack, and keeps it for later rather than only
// for the race detector, passes it through here, as the unwinder does
// (TestGoplusCallerPCReaders keeps the list).
func goplusReturnAddress(gp *g, pc uintptr) uintptr {
	if h := &gp.goplusHijack; h.slot != 0 && pc == h.ret {
		return h.pc
	}
	return pc
}

// goplusLateRefusals counts the refusals goplusRaiseMaxStackLate has set
// up, for tests to tell them from refusals in place.
var goplusLateRefusals atomic.Uint64

// goplusHijackSlot returns the slot on gp's stack whose return address
// goplusRaiseMaxStackLate overwrote, and the return address it stood for,
// or 0 and 0 if there is none or gp is nil. It is for the frame-pointer
// walks, of the execution tracer and the block and mutex profiles, which
// read return addresses without the unwinder: each reads the pc of the
// return address at slot instead.
func goplusHijackSlot(gp *g) (slot, pc uintptr) {
	if gp == nil {
		return 0, 0
	}
	return gp.goplusHijack.slot, gp.goplusHijack.pc
}

// goplusTestRuntimeFunc is a symbol name prefix of functions a test has
// the stack limit treat as runtime functions that compiled code calls, so
// that it can make each growth of a recursion come in a call from the
// runtime on every platform. It is nil outside those tests. It is set
// while other goroutines may be refused, so it is read atomically: a
// string's two words written apart could be read as a length with the
// wrong pointer.
var goplusTestRuntimeFunc atomic.Pointer[string]

// goplusIsTestRuntimeFunc reports whether name starts with
// goplusTestRuntimeFunc's prefix.
func goplusIsTestRuntimeFunc(name string) bool {
	p := goplusTestRuntimeFunc.Load()
	return p != nil && stringslite.HasPrefix(name, *p)
}

// goplusRecovered is called by recovery for gp, which has recovered a
// panic and is to resume in the frame whose stack pointer is sp. If a
// stack growth was refused on gp since its stack was last restored, it
// asks gp to stop at its next function call, where newstack can shrink
// the stack it is running on (goplusRestoreStack). It raises the stack
// guard without setting gp.preempt, so that newstack resumes gp after the
// shrink instead of descheduling it.
//
// A refusal still pending in a frame below sp is forgotten: that frame is
// gone, and its return address will never be returned to.
func goplusRecovered(gp *g, sp uintptr) {
	if gp.stackRestore != 0 {
		gp.stackguard0 = stackPreempt
	}
	if gp.goplusHijack.slot != 0 && gp.goplusHijack.slot < sp {
		gp.goplusHijack = goplusHijack{}
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
// never called. It is entered as though it had been, at a call site in the
// code that called the function whose growth was refused: by
// goplusRaiseMaxStack, at that call, and by goplusHijackReturn, at the
// call to the runtime function that was let complete.
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
