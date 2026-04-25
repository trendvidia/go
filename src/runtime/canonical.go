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
//
// TODO(L2.d follow-up): the mutation path (canonicalSyms[name] = addr
// and slot rewrite) bus-errors during early runtime startup on
// darwin/arm64 — bisection on galaxy showed even slice-append-based
// alternatives fail in the same way. Likely a write-barrier or PAC
// interaction specific to early-startup global mutation. For now
// canonicalizeSlots iterates and reads but does not mutate, leaving
// every slot self-referential. This is correct for single-module
// programs (the host's own slots already point at the host's
// instances); cross-module canonicalisation under -buildmode=plugin
// will need this resolved before L3's Close can rely on it.
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

// canonicalizeSlots walks slots, names, and offsets in lockstep.
// When the canonicalSyms registry mutation issue (see file-level
// TODO) is resolved, this becomes the first-loader-wins
// canonicalisation pass. Until then it's a structural placeholder
// that exercises the iteration and name decode without touching the
// shared registry — slots stay self-referential.
func canonicalizeSlots(slots []uintptr, offs []int32, blob []byte) {
	for i := range slots {
		_ = (*uintptr)(unsafe.Pointer(slots[i]))
		_ = canonicalSymName(blob, offs, i)
	}
}

// globalptrsinit will canonicalise all go.globalptr.* slot
// indirections across every active module to their first-loader-wins
// instance once the registry mutation issue is resolved. Currently
// runs the iteration scaffolding only; slots stay self-referential,
// which is correct for single-module programs.
func globalptrsinit() {
	lock(&canonicalSymsLock)
	for _, md := range activeModules() {
		if len(md.globalptrs) == 0 {
			continue
		}
		canonicalizeSlots(md.globalptrs, md.globalptrnameoffs, md.globalptrnames)
	}
	unlock(&canonicalSymsLock)
}

// funcptrsinit will canonicalise all go.funcptr.* slot indirections
// (closure code pointers, function values, method-value wrappers).
// Currently the structural placeholder; see globalptrsinit and the
// file-level TODO.
func funcptrsinit() {
	lock(&canonicalSymsLock)
	for _, md := range activeModules() {
		if len(md.funcptrs) == 0 {
			continue
		}
		canonicalizeSlots(md.funcptrs, md.funcptrnameoffs, md.funcptrnames)
	}
	unlock(&canonicalSymsLock)
}
