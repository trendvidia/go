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

// pkgKey identifies a specific compiled instance of a Go package.
// name is the package path; hash is the linktimehash distinguishing
// builds of the same path from different source (e.g. each plugin's
// own "main" package, or two plugins built against different versions
// of a shared library).
type pkgKey struct {
	name string
	hash string
}

// pkgOwners records the first-loader of each (package, hash) tuple
// across all currently-loaded modules. When a later module loads with
// the same tuple in its pkghashes, recordPkgDependencies records a
// peer-plugin dependency edge from the later module to the earlier
// owner.
//
// This is how L3 tracks cross-plugin DATA dependencies after Bug D
// removed go.globalptr.* slots: the runtime no longer observes
// dynamically-bound data symbol addresses (the OS dynamic linker
// handles that on every platform), so we track at the package level
// instead. A plugin importing a package owned by a peer plugin
// implicitly depends on that peer regardless of how individual
// symbol bindings are wired by the dynamic linker.
//
// Access is serialised by canonicalSymsLock.
var pkgOwners map[pkgKey]*moduledata

// hostExportsRegistered records whether registerHostExports has run.
// The host's exports never change after startup, so once is enough.
var hostExportsRegistered bool

// registerHostExports enters the host's user-package data exports in
// canonicalSyms, so the host owns those names under first-loader-wins
// (trendvidia/go-archive#106).
//
// The host is firstmoduledata: the main program's module is always the
// first in the list, and a plugin is only ever appended. Its exports
// are registered here rather than in userDataInit because that runs per
// freshly-loaded module and the host is never one of those — its
// carrier was emitted by every Windows link and read by nothing.
//
// Only pass 1 of userDataInit applies. The host has no `__imp_X`
// SNOPTRDATA wrappers to rewrite: those are synthesized by
// peGotRedirect for -dynlink code, and a host is not built that way
// (measured: a host's userDataImports carriers are empty).
//
// Caller holds canonicalSymsLock.
func registerHostExports() {
	if hostExportsRegistered {
		return
	}
	hostExportsRegistered = true
	md := &firstmoduledata
	if len(md.udexports) == 0 {
		return
	}
	if canonicalSyms == nil {
		canonicalSyms = make(map[string]canonicalEntry, 1024)
	}
	for i, addr := range md.udexports {
		name := canonicalSymName(md.udexportnames, md.udexportnameoffs, i)
		if _, ok := canonicalSyms[name]; !ok {
			canonicalSyms[name] = canonicalEntry{addr: addr, owner: md}
		}
	}
}

// userDataInit handles L3.b's peer-plugin DATA canonicalisation. Two
// passes, both under canonicalSymsLock:
//
//  1. Register every user-package data symbol this module DEFINES in
//     canonicalSyms with first-loader-wins semantics. Each entry maps
//     "pkg/path.SymName" → (addr, owner).
//
//  2. Walk every `__imp_X` SNOPTRDATA wrapper this module IMPORTS for
//     a user-package data symbol. If the canonical owner of X is a
//     different module, rewrite the wrapper's content to the canonical
//     address. The compiler-emitted `MOV X@GOTPCREL(%rip), R; MOV (R),
//     value` access pattern in this module's code then loads the
//     canonical address into R and dereferences to the canonical
//     instance — both intra- and cross-package accesses unified.
//
// Caller holds canonicalSymsLock.
func userDataInit(md *moduledata) {
	// canonicalSyms is lazy-initialised by canonicalizeSlots when any
	// module has funcptrs; if every module's funcptrs slice is empty
	// (rare but possible — e.g. plugins that hold no cross-module
	// function values) the map is still nil here. Initialise it so
	// the export-registration pass below has something to write to.
	if len(md.udexports) > 0 && canonicalSyms == nil {
		canonicalSyms = make(map[string]canonicalEntry, 1024)
	}
	// Pass 1: register exports.
	for i, addr := range md.udexports {
		name := canonicalSymName(md.udexportnames, md.udexportnameoffs, i)
		if _, ok := canonicalSyms[name]; !ok {
			canonicalSyms[name] = canonicalEntry{addr: addr, owner: md}
		}
	}

	// Pass 2: rewrite imports whose canonical lives elsewhere.
	for i, slotAddr := range md.udimports {
		name := canonicalSymName(md.udimportnames, md.udimportnameoffs, i)
		entry, ok := canonicalSyms[name]
		if !ok || entry.owner == md {
			continue
		}
		slot := (*uintptr)(unsafe.Pointer(slotAddr))
		if *slot != entry.addr {
			*slot = entry.addr
		}
		recordDependency(md, entry.owner)
	}
}

// userDataAddrsInit rewrites static-data words that embed the address
// of a user-package global (trendvidia/go-archive#25: `var P = &pkg.Global`
// compiled into initialized data, where no slot-routed code access can
// canonicalise the value). The linker paired each word with the
// `__imp_X` slot for its target: for host-shared X that is the IAT
// entry the OS loader filled at LoadLibrary time; for peer-owned X it
// is the SNOPTRDATA wrapper that userDataInit just rewrote to the
// canonical instance. Either way *slot is now X's canonical address,
// so each word becomes *slot plus its embedded addend (`&pkg.Arr[2]`).
//
// Must run after userDataInit(md) — the wrappers' contents are only
// canonical from then on — and the Close-safety dependency edges for
// peer-owned targets were already recorded by userDataInit's wrapper
// walk. Caller holds canonicalSymsLock.
func userDataAddrsInit(md *moduledata) {
	// The three carriers are parallel arrays, but unlike typeptrs /
	// typeptroffs (one linker-side count, two symbols) each of these
	// takes its length from its OWN symbol size in ld/symtab.go's
	// emitSlotSlice, so a linker-side skew shows up as three differing
	// lengths rather than being masked. Name the table, as
	// typeptrsinit does, instead of dying on an index-out-of-range
	// inside plugin_lastmoduleinit with canonicalSymsLock held.
	//
	// This check runs FIRST, ahead of the empty-table return below: a
	// skew that leaves udaddrwords empty while slots/adds are
	// populated is exactly the shape emitSlotSlice's three independent
	// sizes can produce, and an early return placed ahead of the check
	// would swallow it silently.
	if len(md.udaddrslots) != len(md.udaddrwords) || len(md.udaddradds) != len(md.udaddrwords) {
		throw("runtime: udaddrwords / udaddrslots / udaddradds length mismatch")
	}
	// Every target but a Windows plugin: emitAddrCarriers sizes all
	// three carriers at zero, so emitSlotSlice publishes nil slices
	// and the walk below is already a zero-trip loop.
	//
	// Do not delete this as dead weight. Removing it was measured to
	// take a GOFIPS140=v1.26.0 linux/arm64 link from 0 to 9 "ABI0-only
	// symbol referenced as ABIInternal" errors in crypto/internal/
	// fips140 (trendvidia/go-archive#111 carries that measurement). The
	// mechanism is NOT the one this comment originally claimed --
	// deadcode is symbol-granular, and a length test that emits no
	// relocations can neither add nor remove a reachability edge, so
	// an early return cannot shrink any reachable set. Whatever the
	// real cause is, it is unexplained and unpinned: no test on any
	// leg this branch runs fails when the return is removed.
	if len(md.udaddrwords) == 0 {
		return
	}
	for i, wordAddr := range md.udaddrwords {
		word := (*uintptr)(unsafe.Pointer(wordAddr))
		slot := (*uintptr)(unsafe.Pointer(md.udaddrslots[i]))
		*word = *slot + md.udaddradds[i]
	}
}

// recordPkgDependencies walks md's pkghashes and either registers md
// as the first-loader of each (package, hash) tuple or records a
// dependency edge to the earlier owner. Caller holds canonicalSymsLock.
func recordPkgDependencies(md *moduledata) {
	if pkgOwners == nil {
		pkgOwners = make(map[pkgKey]*moduledata)
	}
	for _, ph := range md.pkghashes {
		k := pkgKey{ph.modulename, ph.linktimehash}
		if owner, ok := pkgOwners[k]; ok {
			recordDependency(md, owner)
		} else {
			pkgOwners[k] = md
		}
	}
}

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
