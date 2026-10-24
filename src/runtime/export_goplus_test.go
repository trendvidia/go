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
