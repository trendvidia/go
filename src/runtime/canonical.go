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
//
// Initialised lazily in canonicalizeSlots rather than at package
// init. The Windows plugin DATA-share path (peSharedRuntimeData)
// binds plugin's __imp_runtime.canonicalSyms to host's slot via the
// IAT, so a `make(...)` package-init in plugin would write a fresh
// empty map into host's slot — both clobbering host's already
// canonicalised entries and (on a second plugin load after GC) hitting
// `bad sweepgen in refill` because plugin's allocator path uses an
// mcache whose cached span has a stale sweepgen relative to host's
// shared mheap_. Lazy init under the existing lock sidesteps both.
var canonicalSyms map[string]canonicalEntry
var canonicalSymsLock mutex

// dependentsRoots anchors every per-module dependents map in a
// GC-scanned global so the map and its backing buffer stay live.
//
// moduledata lives in the linker-emitted .go.module section, which is
// writable but explicitly NOT scanned by the GC (see cmd/link/internal/
// ld/data.go's "GC doesn't need to look at it" comment for SMODULEDATA).
// That means the *Map pointer stored in target.dependents is invisible
// to GC: the heap-allocated Map header and its group buffer can be
// reclaimed and the freed memory reused by other allocations, while
// target.dependents still points at it. Symptoms observed before the
// fix: dependents[referencer] lookups missed entries that had just
// been inserted (the group buffer's bytes were overwritten by an
// unrelated map's contents), surfacing as "small map with no empty
// slot (concurrent map writes?)" on the eighth growth-triggering put.
//
// Anchoring each map here keeps it reachable from a real GC root.
// Access is serialised by canonicalSymsLock.
var dependentsRoots []map[*moduledata]struct{}

// canonicalSymName returns the target name for slot index i in the
// per-module name table. Returned string shares storage with the
// module's funcptrnames blob (via unsafe.String);
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
	if canonicalSyms == nil {
		canonicalSyms = make(map[string]canonicalEntry, 1024)
	}
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
		m := make(map[*moduledata]struct{})
		target.dependents = m
		// Anchor m in dependentsRoots so the heap object stays
		// live — see dependentsRoots' comment for why moduledata
		// alone is not a GC root.
		dependentsRoots = append(dependentsRoots, m)
	}
	if _, exists := target.dependents[referencer]; !exists {
		target.dependents[referencer] = struct{}{}
		target.refcount++
	}
}

// funcptrsinit canonicalises go.funcptr.* slot indirections (closure
// code pointers, function values, method-value wrappers) so that
// pointer-identity comparisons on func values work across modules.
//
// (Data-side canonicalisation was previously handled by globalptrsinit
// walking md.globalptrs. That path was removed in the Bug D fix once
// the compiler stopped emitting go.globalptr.* slots — cross-module
// data sharing now goes through the linker's native mechanism on each
// platform: the IAT on Windows, the GOT on POSIX.)
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
