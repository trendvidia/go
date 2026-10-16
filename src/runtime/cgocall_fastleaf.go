// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Calls to C functions declared with #cgo fastleaf (a go-plus addition).

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"internal/strconv"
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

// sysmonCheckFastLeaf throws if a #cgo fastleaf call has run longer than
// GODEBUG=fastleafcheck allows. sysmon calls it on every pass while the
// setting is on, and the call path pays nothing for it: it reads only
// what cgocallLeaf records anyway. (Storing the C function pointer on
// the M as well cost 0.4ns of an 11.4ns call on darwin/arm64, so the
// report names the C function by its cgo wrapper instead.)
//
// A call is identified by its M and that M's ncgocall, which cgocallLeaf
// increments on entry and leaves alone until the next call. A call seen
// on two passes has run at least as long as the time between them, so
// the time reported is a lower bound, and the check's resolution is
// sysmon's wake-up interval: 20us when busy, up to 10ms when idle.
//
// It reads other Ms' state without synchronization, as retake does. A
// call that ends between the reads looks like a different call, or like
// none, and is caught again on the next pass if it is still running.
func sysmonCheckFastLeaf(now int64) {
	limit := debug.fastleafcheck
	lock(&allpLock)
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		pd := &pp.sysmontick
		mp := pp.m.ptr()
		if atomic.Load(&pp.status) != _Prunning || mp == nil {
			pd.leafwhen = 0
			continue
		}
		call := mp.ncgocall
		gp := mp.curg
		if gp == nil || !gp.cgoFastLeaf {
			pd.leafwhen = 0
			continue
		}
		if pd.leafwhen == 0 || pd.leafm.ptr() != mp || pd.leafcall != call {
			pd.leafm.set(mp)
			pd.leafcall = call
			pd.leafwhen = now
			continue
		}
		if now-pd.leafwhen < limit {
			continue
		}
		// The Go wrapper cgo generated for the C function (_Cfunc_name)
		// is cgocallLeaf's caller, and names it without a C symbolizer.
		wrapper := "an unknown function"
		if f := findfunc(gp.syscallpc); f.valid() {
			wrapper = funcname(f)
		}
		printlock()
		print("runtime: #cgo fastleaf call to ", wrapper, " in goroutine ", gp.goid, " has run for at least ")
		printDurationNS(now - pd.leafwhen)
		print(", over GODEBUG=fastleafcheck=")
		printDurationNS(limit)
		print("\n")
		printunlock()
		unlock(&allpLock)
		throw("#cgo fastleaf call ran too long")
	}
	unlock(&allpLock)
}

// printDurationNS prints a duration in nanoseconds, in milliseconds
// when it is at least one.
func printDurationNS(ns int64) {
	switch {
	case ns >= 1e6:
		print(ns/1e6, "ms")
	case ns >= 1e3:
		print(ns/1e3, "us")
	default:
		print(ns, "ns")
	}
}

// parseFastLeafCheck parses the value of GODEBUG=fastleafcheck: a
// duration with a unit (ns, us, ms, s), or 0 to turn the check off.
func parseFastLeafCheck(s string) (int64, bool) {
	if s == "0" {
		return 0, true
	}
	i := 0
	for i < len(s) && '0' <= s[i] && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	var unit int64
	switch s[i:] {
	case "ns":
		unit = 1
	case "us", "\u00b5s":
		unit = 1e3
	case "ms":
		unit = 1e6
	case "s":
		unit = 1e9
	default:
		return 0, false
	}
	if n > (1<<63-1)/unit {
		return 0, false
	}
	return n * unit, true
}
