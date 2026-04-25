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
// canonicalSyms persists across module loads. Plugin Close (Layer 3)
// is responsible for any per-module entry cleanup; the table itself
// outlives individual modules.
//
// Access is serialised by canonicalSymsLock. The map is created
// lazily by globalptrsinit on first call.
var canonicalSyms map[string]uintptr
var canonicalSymsLock mutex

// canonicalSymName returns the target name for slot index i in tab,
// where tab is one of moduledata.globalptrnameoffs / funcptrnameoffs
// and blob is the matching globalptrnames / funcptrnames byte slice.
// The name is a substring of the blob; valid for the lifetime of the
// module.
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
// Caller holds canonicalSymsLock. slots[i] is a pointer-sized writable
// memory location whose static initializer is the address of the
// local module's instance of the target named at offs[i] in blob.
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

// globalptrsinit canonicalises all go.globalptr.* slot indirections
// across every active module to their first-loader-wins instance.
// Run once per module load (host startup and each plugin load) after
// modulesinit; cheap and idempotent on already-canonicalised slots,
// so a host that re-runs it after plugin loads incurs no double-work
// beyond the slot scan itself.
func globalptrsinit() {
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

// funcptrsinit canonicalises all go.funcptr.* slot indirections
// (closure code pointers, function values, method-value wrappers) the
// same way globalptrsinit handles data globals. Run alongside
// globalptrsinit at every module load.
func funcptrsinit() {
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
