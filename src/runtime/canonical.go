// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// canonicalEntry records the canonical (first-loader-wins) address
// of a target symbol along with the module that owns it. Recording
// the owner is what lets L3 build the per-module dependents set:
// when canonicalizeSlots rewrites a referencer's slot to point at
// owner's address, we know to record (referencer → owner) as a
// dependency edge.
type canonicalEntry struct {
	addr  uintptr
	owner *moduledata
}

// canonicalSyms maps a symbol's target name (the string after the
// "go.globalptr." or "go.funcptr." prefix in a slot symbol's name)
// to the canonical (addr, owner). The first module to register a
// name owns the canonical address; subsequent modules' slots are
// rewritten to that address (first-loader-wins).
//
// canonicalSyms persists across module loads. Plugin Close (L3.b)
// removes the closing module's owner entries; remaining entries
// outlive individual modules.
//
// Access is serialised by canonicalSymsLock.
var canonicalSyms = make(map[string]canonicalEntry, 1024)
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
// cur is the module owning these slots. When a slot is rewritten to
// point at another module's instance, we record that cur depends on
// that other module — used by Plugin.Close to refuse unload of a
// module whose symbols are still referenced.
//
// Caller holds canonicalSymsLock.
func canonicalizeSlots(cur *moduledata, slots []uintptr, offs []int32, blob []byte) {
	for i := range slots {
		slot := (*uintptr)(unsafe.Pointer(slots[i]))
		name := canonicalSymName(blob, offs, i)
		if entry, ok := canonicalSyms[name]; ok {
			if *slot != entry.addr {
				*slot = entry.addr
			}
			recordDependency(cur, entry.owner)
			continue
		}
		canonicalSyms[name] = canonicalEntry{addr: *slot, owner: cur}
	}
}

// recordDependency notes that referencer holds a canonicalised
// reference into target's symbols. Self-references (cur == owner of
// canonical, common when iterating the FIRST loader of a name) are
// no-ops. The dependent set is allocated lazily on first edge.
//
// Caller holds canonicalSymsLock so the lazy allocation and map
// write are race-free.
func recordDependency(referencer, target *moduledata) {
	if referencer == target {
		return
	}
	if target.dependents == nil {
		target.dependents = make(map[*moduledata]struct{})
	}
	if _, exists := target.dependents[referencer]; !exists {
		target.dependents[referencer] = struct{}{}
		target.refcount++
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
	for _, md := range activeModules() {
		if len(md.globalptrs) == 0 {
			continue
		}
		canonicalizeSlots(md, md.globalptrs, md.globalptrnameoffs, md.globalptrnames)
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
	for _, md := range activeModules() {
		if len(md.funcptrs) == 0 {
			continue
		}
		canonicalizeSlots(md, md.funcptrs, md.funcptrnameoffs, md.funcptrnames)
	}
	unlock(&canonicalSymsLock)
}
