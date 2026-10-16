// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Calls to C functions declared with #cgo fastleaf (a go-plus addition).

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

// cgocallLeaf calls the C function fn, declared #cgo fastleaf, with
// argument frame arg.
//
// It is cgocall without the scheduler handoff: no entersyscall or
// exitsyscall, so the goroutine keeps its P and stays running while fn
// runs on the system stack. That is only sound for a leaf: fn must be
// short, must not block, and must not call back into Go. A fastleaf
// function that blocks holds its P for as long as it blocks, and delays
// a stop-the-world until it returns, because a goroutine running C code
// is not at a safe point. A callback into Go throws (see cgocallbackg).
//
// Like cgocall, it is nosplit: the arguments are untyped on the stack.
//
// Apart from the system call, its bookkeeping is cgocall's, copied rather
// than shared: a helper for the DIT restore is not inlined and costs
// about 0.2ns on every cgo call. A change to cgocall's bookkeeping must
// be made here too; TestFastLeafDIT in cmd/cgo/internal/testfastleaf pins
// the DIT half.
//
//go:linkname cgocallLeaf
//go:nosplit
func cgocallLeaf(fn, arg unsafe.Pointer) int32 {
	if !iscgo && GOOS != "solaris" && GOOS != "illumos" && GOOS != "windows" {
		throw("cgocall unavailable")
	}
	if fn == nil {
		throw("cgocall nil")
	}
	if raceenabled {
		racereleasemerge(unsafe.Pointer(&racecgosync))
	}

	gp := getg()
	mp := gp.m
	mp.ncgocall++
	mp.cgoCallers[0] = 0

	// Record where the goroutine's Go stack stops, as entersyscall does
	// for an ordinary cgo call. The goroutine stays running, but its Go
	// stack is frozen below this point until the call returns, and the
	// code that unwinds a goroutine in C starts from here: the CPU
	// profiler (sigprof) and crash tracebacks (traceback1), which would
	// otherwise unwind from the C function's PC on the system stack.
	fp := getcallerfp()
	gp.syscallsp = sys.GetCallerSP()
	gp.syscallpc = sys.GetCallerPC()
	gp.syscallbp = fp

	// As in cgocall: tell asynchronous preemption that the thread is in
	// external code, and mark C on the stack for tracing and profiling.
	osPreemptExtEnter(mp)
	mp.incgo = true
	mp.ncgo++
	gp.cgoFastLeaf = true

	errno := asmcgocall(fn, arg)

	gp.syscallsp = 0
	gp.syscallpc = 0
	gp.syscallbp = 0
	gp.cgoFastLeaf = false
	mp.incgo = false
	mp.ncgo--
	osPreemptExtExit(mp)

	if raceenabled {
		raceacquire(unsafe.Pointer(&racecgosync))
	}
	if sys.DITSupported {
		// C code may have changed DIT on this thread; restore ours.
		ditEnabled := sys.DITEnabled()
		if !gp.ditWanted && ditEnabled {
			sys.DisableDIT()
		} else if gp.ditWanted && !ditEnabled {
			sys.EnableDIT()
		}
	}
	return errno
}
