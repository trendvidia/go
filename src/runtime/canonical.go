// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// canonicalSyms maps a symbol's target name (the string after the
// "go.globalptr." or "go.funcptr." prefix in a slot symbol's name) to
// the canonical address of that target across all loaded modules.
// The first module to register a name owns the canonical address;
// subsequent modules' slots are rewritten to point at the registered
// address (first-loader-wins).
//
// canonicalSyms is allocated lazily on the FIRST plugin load —
// single-module programs never touch it. This sidesteps an early-
// runtime heap-allocator state issue that surfaces when a map is
// grown from inside schedinit before the page allocator is fully
// settled (typelinksinit avoids the same problem with its own
// firstmoduledata.next == nil short-circuit).
//
// On the first plugin load, globalptrsinit walks ALL active modules:
// host first (since modulesinit places the hasmain module at index
// 0), then the plugin. Host's slot values get registered as
// canonical; plugin's slot values get rewritten to the registered
// canonical. Subsequent plugin loads canonicalise just the new
// plugin against the existing registry.
//
// Access is serialised by canonicalSymsLock.
var canonicalSyms map[string]uintptr
var canonicalSymsLock mutex

// canonicalSymName returns the target name for slot index i in the
// per-module name table. Returned string shares storage with the
// module's globalptrnames / funcptrnames blob (via unsafe.String);
// valid for the lifetime of the module.
func canonicalSymName(blob []byte, offs []int32, i int) string {
	off := int(offs[i])
	end := off
	for end < len(blob) && blob[end] != 0 {
		end++
	}
	return unsafe.String(&blob[off], end-off)
}

// canonicalizeSlots walks slots, names, and offsets in lockstep,
// registering each previously-unseen target name's address as
// canonical and rewriting subsequent loaders' slots to point at it.
//
// Caller holds canonicalSymsLock and has already verified that more
// than one module is loaded (so the canonicalSyms map is safe to
// allocate and grow).
func canonicalizeSlots(slots []uintptr, offs []int32, blob []byte) {
	for i := range slots {
		slot := (*uintptr)(unsafe.Pointer(slots[i]))
		name := canonicalSymName(blob, offs, i)
		if canonical, ok := canonicalSyms[name]; ok {
			if *slot != canonical {
				*slot = canonical
			}
			continue
		}
		canonicalSyms[name] = *slot
	}
}

// globalptrsinit canonicalises go.globalptr.* slot indirections
// across every active module to their first-loader-wins instance.
// Run at every module load (host startup and each plugin load),
// but short-circuits for single-module programs where slots are
// already self-referential and no cross-module canonicalisation is
// possible.
func globalptrsinit() {
	if firstmoduledata.next == nil {
		// Single-module program; slots already self-referential.
		return
	}
	lock(&canonicalSymsLock)
	if canonicalSyms == nil {
		canonicalSyms = make(map[string]uintptr)
	}
	for _, md := range activeModules() {
		if len(md.globalptrs) == 0 {
			continue
		}
		canonicalizeSlots(md.globalptrs, md.globalptrnameoffs, md.globalptrnames)
	}
	unlock(&canonicalSymsLock)
}

// funcptrsinit canonicalises go.funcptr.* slot indirections (closure
// code pointers, function values, method-value wrappers) the same
// way globalptrsinit handles data globals.
func funcptrsinit() {
	if firstmoduledata.next == nil {
		return
	}
	lock(&canonicalSymsLock)
	if canonicalSyms == nil {
		canonicalSyms = make(map[string]uintptr)
	}
	for _, md := range activeModules() {
		if len(md.funcptrs) == 0 {
			continue
		}
		canonicalizeSlots(md.funcptrs, md.funcptrnameoffs, md.funcptrnames)
	}
	unlock(&canonicalSymsLock)
}
