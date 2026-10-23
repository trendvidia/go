// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

var GoplusSampleWeight = goplusSampleWeight

// Fastexprand draws the gap to the heap profiler's next sample.
func Fastexprand(mean int) int32 { return fastexprand(mean) }

var GoplusRuntimeOwned = goplusRuntimeOwned

// GoplusPanicSafeAt reports whether the stack limit may refuse a call
// whose return address is pc.
func GoplusPanicSafeAt(pc uintptr) bool { return goplusPanicSafeAt(pc) }

var GoplusMapWriter = goplusMapWriter

// GoplusInMapWrite runs goplusInMapWrite's walk over a stack of functions
// with the given symbol names, innermost first, and reports whether it
// finds a map write.
func GoplusInMapWrite(stack ...string) bool {
	for _, name := range stack {
		if inMap, done := goplusMapWriteFrame(name); done {
			return inMap
		}
	}
	return false
}

var GoplusLateRefusable = goplusLateRefusable

var GoplusLinknamedStd = goplusLinknamedStd[:]

// GoplusLateRefusals returns how many stack-limit refusals have waited
// for a runtime call to return.
func GoplusLateRefusals() uint64 { return goplusLateRefusals.Load() }

// SetGoplusTestRuntimeFunc has the stack limit treat the functions whose
// symbol names start with prefix as runtime functions compiled code calls;
// "" undoes it.
func SetGoplusTestRuntimeFunc(prefix string) {
	if prefix == "" {
		goplusTestRuntimeFunc.Store(nil)
		return
	}
	p := new(string)
	*p = prefix
	goplusTestRuntimeFunc.Store(p)
}

// GoplusLateRefusalPending reports whether a refusal is waiting for a
// runtime call to return on the calling goroutine.
func GoplusLateRefusalPending() bool { return getg().goplusHijack.slot != 0 }

// GoplusLateRefusalOnStack reports whether the return address a pending
// refusal overwrote is on the calling goroutine's current stack.
func GoplusLateRefusalOnStack() bool {
	gp := getg()
	s := gp.goplusHijack.slot
	return s != 0 && gp.stack.lo <= s && s < gp.stack.hi
}

var GoplusOrdinaryName = goplusOrdinaryName
