// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/stringslite"
	"unsafe"
)

// plugin_validatePluginPkghashes validates a freshly-loaded Windows
// plugin's per-package fingerprints against every module already in
// the address space: the host first, then any loaded peer plugin.
// addr is the address of the plugin's go:plugin.pkghashes export — a
// slice header (ptr, len, cap) referencing the plugin's
// go:link.pkghashes carrier (a []modulehash).
//
// For each candidate entry we look up the same package in
// firstmoduledata.pkghashes (the host's carrier, populated by symtab.go
// for hosts that import the plugin package) and compare linktimehash;
// a difference names the host as the stale side. If the host does not
// link the package at all, the entry is compared against every other
// loaded module's own pkghashes (trendvidia/go#24): two plugins built
// from different sources of a host-absent package must not both load,
// because the cross-module binding machinery (canonicalSyms /
// userDataInit, keyed by bare symbol name) would silently converge the
// second loader onto the first loader's instance. On linux/darwin this
// peer fence comes for free — ld.so rebinds each pkghash.runtimehash
// pointer to the first definition in the global namespace, which
// plugin_lastmoduleinit then compares against — but Windows PE has no
// equivalent, so the scan here replaces it.
//
// A mismatch returns an error string naming the offending package and
// both sides' fingerprints; absent everywhere is OK.
//
// Empty addr (plugin built with an older toolchain that lacks the
// export) returns "" so loading proceeds without a check, matching the
// pre-fix behavior.
//
//go:linkname plugin_validatePkghashes plugin.validatePkghashes
func plugin_validatePkghashes(addr uintptr) (errstr, pluginpath string) {
	if addr == 0 {
		return "", ""
	}
	hdr := (*[3]uintptr)(unsafe.Pointer(addr))
	n := hdr[1]
	if n == 0 {
		return "", ""
	}
	pluginEntries := unsafe.Slice((*modulehash)(unsafe.Pointer(hdr[0])), int(n))
	// The candidate is already chained — its DllMain ran addmoduledata
	// before this check (open() verified the chain grew across
	// LoadLibrary, so the newest node is this plugin's). It is the last
	// non-bad module: exclude it from the peer scan below, and identify
	// it for detachment on mismatch.
	var cand *moduledata
	for pmd := firstmoduledata.next; pmd != nil; pmd = pmd.next {
		if !pmd.bad {
			cand = pmd
		}
	}
	var mismatchPkg, pluginHash, otherHash, peerPath string
	for i := range pluginEntries {
		e := &pluginEntries[i]
		hostHas := false
		for j := range firstmoduledata.pkghashes {
			h := &firstmoduledata.pkghashes[j]
			if h.modulename != e.modulename {
				continue
			}
			hostHas = true
			if h.linktimehash != e.linktimehash {
				mismatchPkg, pluginHash = e.modulename, e.linktimehash
				otherHash = h.linktimehash
			}
			break
		}
		if mismatchPkg != "" {
			break
		}
		if hostHas {
			continue
		}
		// Host-absent: compare against loaded peers (trendvidia/go#24).
		for pmd := firstmoduledata.next; pmd != nil; pmd = pmd.next {
			if pmd.bad || pmd == cand {
				continue
			}
			for k := range pmd.pkghashes {
				ph := &pmd.pkghashes[k]
				if ph.modulename == e.modulename && ph.linktimehash != e.linktimehash {
					mismatchPkg, pluginHash = e.modulename, e.linktimehash
					otherHash, peerPath = ph.linktimehash, pmd.pluginpath
					break
				}
			}
			if mismatchPkg != "" {
				break
			}
		}
		if mismatchPkg != "" {
			break
		}
	}
	if mismatchPkg == "" {
		return "", ""
	}
	// Name which already-loaded module holds the divergent build:
	// "host", or the peer plugin's path.
	side := "host"
	if peerPath != "" {
		side = "peer " + peerPath
	}
	msg := pluginVersionMismatchMsg(mismatchPkg, pluginHash, side, otherHash)
	// Mark the just-loaded plugin's moduledata as bad and detach it
	// from the chain. resolveTypeOff / resolveNameOff / textOff in
	// runtime/type.go skip bad modules so the half-initialized typemap
	// of a rejected plugin doesn't poison subsequent reflect
	// operations; detaching goes further so the caller can FreeLibrary
	// the rejected DLL (trendvidia/go#26) without leaving dangling
	// chain entries — the moduledata lives inside the DLL's image.
	// Nothing registered this module under canonicalSyms/pkgOwners
	// (validation precedes plugin_lastmoduleinit), so there is nothing
	// else to unwind. Caller holds pluginsMu, which serialises chain
	// mutation against concurrent loads.
	if cand != nil {
		cand.bad = true
		detachModule(cand)
		modulesinit()
		return msg, cand.pluginpath
	}
	return msg, ""
}

// pluginVersionMismatchMsg formats the plugin.Open version-lock error.
// The version lock spans every package linked into both host and
// plugin — not just govm's CompilerVersion — so a bare package name is
// often not enough to diagnose which side is stale. We name the
// package and both build fingerprints so a standalone plugin build
// that resolved a package differently from an already-loaded module is
// diagnosable without decoding the raw plugin.Open message or
// re-deriving hashes by hand. See trendvidia/go#7 and trendvidia/go#24.
//
// side names the already-loaded holder of the divergent build:
// "host" for the host carrier, "peer <pluginpath>" when a previously-
// loaded plugin owns it (trendvidia/go#24), or "loaded" where the
// check cannot tell which module a rebound runtimehash pointer
// resolved against (the POSIX path in plugin_lastmoduleinit).
//
// linktimehash carries the raw package fingerprint bytes (see the
// linker's go:link.pkghash emission in cmd/link/internal/ld/symtab.go),
// which are not printable, so both are rendered as hex.
func pluginVersionMismatchMsg(pkg, pluginHash, side, hash string) string {
	return "plugin was built with a different version of package " + pkg +
		" (plugin build hash " + pkghashHex(pluginHash) +
		", " + side + " build hash " + pkghashHex(hash) + ")"
}

// pkghashHex renders a raw package fingerprint (the byte string carried
// in modulehash.linktimehash / *runtimehash) as lowercase hex for use
// in diagnostics. The fingerprints are raw bytes rather than text, so
// they must be encoded before appearing in an error message.
func pkghashHex(s string) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		b = append(b, digits[s[i]>>4], digits[s[i]&0xf])
	}
	return string(b)
}

//go:linkname plugin_lastmoduleinit plugin.lastmoduleinit
func plugin_lastmoduleinit() (path string, syms map[string]any, initTasks []*initTask, errstr string) {
	var md *moduledata
	for pmd := firstmoduledata.next; pmd != nil; pmd = pmd.next {
		if pmd.bad {
			md = nil // we only want the last module
			continue
		}
		md = pmd
	}
	if md == nil {
		throw("runtime: no plugin module data")
	}
	if md.pluginpath == "" {
		throw("runtime: plugin has empty pluginpath")
	}
	if md.typemap != nil {
		return "", nil, nil, "plugin already loaded"
	}

	for _, pmd := range activeModules() {
		if pmd.pluginpath == md.pluginpath {
			md.bad = true
			return "", nil, nil, "plugin already loaded"
		}

		if inRange(pmd.text, pmd.etext, md.text, md.etext) ||
			inRange(pmd.bss, pmd.ebss, md.bss, md.ebss) ||
			inRange(pmd.data, pmd.edata, md.data, md.edata) ||
			inRange(pmd.types, pmd.etypes, md.types, md.etypes) {
			println("plugin: new module data overlaps with previous moduledata")
			println("\tpmd.text-etext=", hex(pmd.text), "-", hex(pmd.etext))
			println("\tpmd.bss-ebss=", hex(pmd.bss), "-", hex(pmd.ebss))
			println("\tpmd.data-edata=", hex(pmd.data), "-", hex(pmd.edata))
			println("\tpmd.types-etypes=", hex(pmd.types), "-", hex(pmd.etypes))
			println("\tmd.text-etext=", hex(md.text), "-", hex(md.etext))
			println("\tmd.bss-ebss=", hex(md.bss), "-", hex(md.ebss))
			println("\tmd.data-edata=", hex(md.data), "-", hex(md.edata))
			println("\tmd.types-etypes=", hex(md.types), "-", hex(md.etypes))
			throw("plugin: new module data overlaps with previous moduledata")
		}
	}
	for _, pkghash := range md.pkghashes {
		if pkghash.linktimehash != *pkghash.runtimehash {
			md.bad = true
			return "", nil, nil, pluginVersionMismatchMsg(pkghash.modulename, pkghash.linktimehash, "loaded", *pkghash.runtimehash)
		}
	}

	// Initialize the freshly loaded module.
	modulesinit()
	funcptrsinit()
	typelinksinit()
	typeptrsinit(md)

	// L3.b cross-module wiring under canonicalSymsLock:
	//   - userDataInit: register this module's user-package data
	//     exports as canonical and rewrite its `__imp_X` import slots
	//     to point at the canonical instance when it lives elsewhere.
	//   - recordPkgDependencies: package-level (pkg path, linktimehash)
	//     ownership tracking — the safety-net for Plugin.Close that
	//     covers symbol kinds (types, function bodies) which the
	//     symbol-level walks above don't directly canonicalise.
	lock(&canonicalSymsLock)
	userDataInit(md)
	recordPkgDependencies(md)
	unlock(&canonicalSymsLock)

	pluginftabverify(md)
	moduledataverify1(md)

	lock(&itabLock)
	for _, i := range md.itablinks {
		itabAdd(i)
	}
	unlock(&itabLock)

	// Redirect go.itabptr.X slots to canonical itabs. Must run AFTER
	// itabAdd above so getitab finds existing entries, and AFTER
	// typeptrsinit so itab Inter/Type canonicalize through typemap.
	itabptrsinit(md)

	// Build a map of symbol names to symbols. Here in the runtime
	// we fill out the first word of the interface, the type. We
	// pass these zero value interfaces to the plugin package,
	// where the symbol value is filled in (usually via cgo).
	//
	// Because functions are handled specially in the plugin package,
	// function symbol names are prefixed here with '.' to avoid
	// a dependency on the reflect package.
	syms = make(map[string]any, len(md.ptab))
	for _, ptab := range md.ptab {
		symName := resolveNameOff(unsafe.Pointer(md.types), ptab.name)
		t := toRType((*_type)(unsafe.Pointer(md.types))).typeOff(ptab.typ) // TODO can this stack of conversions be simpler?
		var val any
		valp := (*[2]unsafe.Pointer)(unsafe.Pointer(&val))
		(*valp)[0] = unsafe.Pointer(t)

		name := symName.Name()
		if t.Kind() == abi.Func {
			name = "." + name
		}
		syms[name] = val
	}
	skipHostSharedInits(md)
	return md.pluginpath, syms, md.inittasks, ""
}

// pluginSkipInitPkg reports whether a package's plugin-local init must
// be short-circuited because its data is host-shared. Pairs with the
// linker's peSharedRuntimeData entry for the package's globals.
//
// Implemented as a switch (not a map) so this list adds no package-
// init allocation to the runtime — a runtime-init-time map allocation
// triggers `bad sweepgen in refill` when plugin's runtime evaluates it
// during DLL load before its allocator is fully ready.
//
// Currently testplugin-only. Backlog: have the linker emit a per-
// plugin side-table of skip-init package paths so this list does not
// have to be maintained by hand against peSharedRuntimeData.
func pluginSkipInitPkg(pkg string) bool {
	switch pkg {
	case "testplugin/common":
		return true
	case "runtime":
		// Plugin's runtime package is host-shared via IAT — every
		// scheduler/GC/finalizer global lives in host's instance, not
		// the plugin's. Plugin's runtime init functions would spawn
		// duplicate forcegchelper / sysmon / scavenger goroutines and
		// re-initialise lock state that host already set up; e.g.
		// runtime.init.7 spawns forcegchelper which then throws
		// "forcegc: phase error" because forcegc.idle is already false
		// from host's helper. Skip on Windows where the host-share
		// model applies; on POSIX plugins are independent runtimes
		// and need their own init.
		return GOOS == "windows"
	}
	return false
}

// skipHostSharedInits sets state = 2 (Done) on plugin-local inittasks
// whose init code lives in the host. Two ways an inittask qualifies:
//
//  1. Its first PC resolves to a `.rel` thunk (a 6-byte JMP-via-IAT
//     stub the linker emits for cross-module calls). On Windows v3,
//     every host-shared package's inittask has its first init function
//     pointing at such a thunk because the actual init lives in host.
//     Plugin must NOT re-run host's init — doing so spawns duplicate
//     forcegchelper / sysmon / scavenger goroutines (runtime.init.7
//     specifically), re-acquires already-acquired runtime locks, and
//     races with host's already-initialised state.
//
//  2. Its package name matches pluginSkipInitPkg's allowlist. Used
//     for cases where the inittask's first PC is plugin-local but
//     the package's data is still host-shared (e.g. testplugin/common
//     where peSharedRuntimeData covers common.X).
//
// On linux/darwin the first PC isn't a thunk (no IAT plumbing), so
// the .rel-suffix branch is a no-op there — only the pluginSkipInitPkg
// allowlist applies.
func skipHostSharedInits(md *moduledata) {
	for _, t := range md.inittasks {
		if t == nil || t.state == 2 || t.nfns == 0 {
			continue
		}
		firstFunc := add(unsafe.Pointer(t), 8)
		f := *(*func())(unsafe.Pointer(&firstFunc))
		fi := findfunc(abi.FuncPCABIInternal(f))
		name := funcname(fi)
		// Windows host-shared init: first PC is in a `.rel` IAT
		// thunk, so the funcname returns ".rel" (no package prefix).
		// These tasks' real bodies are in host; skip.
		if name == ".rel" {
			t.state = 2
			continue
		}
		if pluginSkipInitPkg(pkgFromInitName(name)) {
			t.state = 2
		}
	}
}

// pkgFromInitName extracts a package path from an init-function name
// like "testplugin/common.init.0" or "runtime.init". The compiler-
// emitted form is `<pkg>.init` or `<pkg>.init.<n>`. We split on the
// first '.' that is not part of the package path. Package paths in
// Go contain '/' but never '.', so the last '/' partitions package
// from function and the first '.' after that begins the function
// name suffix.
func pkgFromInitName(name string) string {
	slash := -1
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			slash = i
		}
	}
	for i := slash + 1; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

func pluginftabverify(md *moduledata) {
	badtable := false
	for i := 0; i < len(md.ftab); i++ {
		entry := md.textAddr(md.ftab[i].entryoff)
		if md.minpc <= entry && entry <= md.maxpc {
			continue
		}

		f := funcInfo{(*_func)(unsafe.Pointer(&md.pclntable[md.ftab[i].funcoff])), md}
		name := funcname(f)

		// A common bug is f.entry has a relocation to a duplicate
		// function symbol, meaning if we search for its PC we get
		// a valid entry with a name that is useful for debugging.
		name2 := "none"
		entry2 := uintptr(0)
		f2 := findfunc(entry)
		if f2.valid() {
			name2 = funcname(f2)
			entry2 = f2.entry()
		}
		badtable = true
		println("ftab entry", hex(entry), "/", hex(entry2), ": ",
			name, "/", name2, "outside pc range:[", hex(md.minpc), ",", hex(md.maxpc), "], modulename=", md.modulename, ", pluginpath=", md.pluginpath)
	}
	if badtable {
		throw("runtime: plugin has bad symbol table")
	}
}

// inRange reports whether v0 or v1 are in the range [r0, r1].
func inRange(r0, r1, v0, v1 uintptr) bool {
	return (v0 >= r0 && v0 <= r1) || (v1 >= r0 && v1 <= r1)
}

// A ptabEntry is generated by the compiler for each exported function
// and global variable in the main package of a plugin. It is used to
// initialize the plugin module's symbol map.
type ptabEntry struct {
	name nameOff
	typ  typeOff
}

// findPluginModule walks the active modules list, returning the
// moduledata whose pluginpath matches. nil if no match. The caller
// holds canonicalSymsLock; we don't lock here to avoid lock-ordering
// concerns between canonicalSymsLock and modulesSlice's atomicity
// (modulesSlice is read via activeModules without taking a lock —
// it's atomic-store/atomic-load).
func findPluginModule(pluginpath string) *moduledata {
	for _, md := range activeModules() {
		if md.pluginpath == pluginpath {
			return md
		}
	}
	return nil
}

//go:linkname plugin_hasDependents plugin.pluginHasDependents
func plugin_hasDependents(pluginpath string) bool {
	lock(&canonicalSymsLock)
	md := findPluginModule(pluginpath)
	r := md != nil && md.refcount > 0
	unlock(&canonicalSymsLock)
	return r
}

//go:linkname plugin_dependents plugin.pluginDependents
func plugin_dependents(pluginpath string) []string {
	lock(&canonicalSymsLock)
	defer unlock(&canonicalSymsLock)
	md := findPluginModule(pluginpath)
	if md == nil || len(md.dependents) == 0 {
		return nil
	}
	out := make([]string, 0, len(md.dependents))
	for dep := range md.dependents {
		// Filter out the host (which has no pluginpath); callers
		// use Dependents() to know which other plugins to Close
		// first, and the host doesn't unload.
		if dep.pluginpath != "" {
			out = append(out, dep.pluginpath)
		}
	}
	return out
}

// pluginClosedFuncStub is the target written into every closure
// descriptor's F field at Plugin.Close. A held func value (e.g. one
// returned by plugin.Lookup, or one referenced by a plugin-internal
// global the host obtained through Lookup-of-variable) calls indirect
// through descriptor.F; after the rewrite that indirect lands here
// and panics with a recognisable message.
//
// The rewrite reaches only registered descriptors: static ·f
// descriptors recorded in canonicalSyms, and the per-Lookup heap
// holders threaded through plugin.Plugin.funcDescriptors. A closure
// created at runtime inside the plugin and returned to the host is
// registered nowhere, keeps pointing at the plugin's text, and kills
// the process with an unrecoverable fault when called after Close —
// as does dereferencing a pointer to any of the plugin's variables.
// See plugin.Plugin.Close for the full envelope.
//
// The stub takes no arguments and returns nothing — caller's actual
// signature can be anything. The ABI mismatch is harmless: panic
// unwinds via deferred handlers before the caller's epilogue runs,
// so the caller never observes a return.
//
// Without this rewrite, a held call after dlclose would jump into
// unmapped memory and SIGSEGV; with it, the failure is an ordinary
// (recoverable) Go panic. v2 ships this together with the descriptor-
// writability change in cmd/compile/internal/staticdata.WriteFuncSyms
// — the rewrite at Close requires the target descriptor to live in
// writable .noptrdata rather than RODATA.
func pluginClosedFuncStub() {
	panic(pluginClosedFuncMsg)
}

const pluginClosedFuncMsg = "plugin: function value invoked after Plugin.Close"

// funcSymSuffix is the string a closure-descriptor symbol's name ends
// with: U+00B7 MIDDLE DOT (0xC2 0xB7 in UTF-8) followed by 'f', i.e.
// "·f". cmd/compile/internal/ir.FuncSymName forms descriptor names
// this way and the linker preserves the suffix into the per-module
// funcptrnames blob, where canonicalSyms uses the (suffix-bearing)
// name as its key. Filtering on this suffix at Close distinguishes
// PFUNC slots (descriptor addresses, what we want to rewrite) from
// OCFUNC slots (raw code addresses, irrelevant for stub conversion).
const funcSymSuffix = "·f"

// neutraliseStaticDescriptors rewrites every closure descriptor that
// physically lives in md's writable .noptrdata so its F field points
// at pluginClosedFuncStub. Called from plugin_close before the
// canonicalSyms cleanup tears down the (name, addr, owner) triples.
//
// canonicalSyms is keyed by descriptor name (including the "·f"
// suffix); an entry's addr is the address of the descriptor itself
// (canonicalizeSlots stored *slot, and the slot was initialised by
// SymPtr to a descriptor symbol).
//
// Two filters apply:
//
//   - owner == md: this module was the first walker to register the
//     name. Other modules' walks share this entry by reading addr;
//     a different module's Close handles its own owner=that-module
//     entries. (Heap holders live elsewhere and are processed via
//     plugin_close's funcDescriptors slice, not here.)
//
//   - addr falls within md's own .noptrdata or .data: the rewrite
//     target must physically be ours. The canonical addr can land in
//     another module's address space when ld.so resolved md's slot
//     to a same-named descriptor over there — typically the host's
//     RODATA copy of a stdlib descriptor (e.g. reflect.cvtDirect·f),
//     since the plugin-importing host emits the symbol but, being
//     non-dynlink, lands it read-only. Writing through such an addr
//     SIGSEGVs even though owner==md is true. The descriptor lives
//     in still-mapped memory (host stays loaded) so neutralisation
//     is also unnecessary: a held call lands in code that is not
//     being unmapped by this Close.
//
// Caller holds canonicalSymsLock.
func neutraliseStaticDescriptors(md *moduledata, stubPC uintptr) {
	inMdData := func(addr uintptr) bool {
		if addr >= md.noptrdata && addr < md.enoptrdata {
			return true
		}
		if addr >= md.data && addr < md.edata {
			return true
		}
		return false
	}
	for name, entry := range canonicalSyms {
		if entry.owner != md {
			continue
		}
		if !stringslite.HasSuffix(name, funcSymSuffix) {
			continue
		}
		if !inMdData(entry.addr) {
			continue
		}
		descF := (*uintptr)(unsafe.Pointer(entry.addr))
		*descF = stubPC
	}
}

//go:linkname plugin_close plugin.pluginClose
func plugin_close(pluginpath string, heapDescriptors []*uintptr) (errstr string) {
	lock(&canonicalSymsLock)
	defer unlock(&canonicalSymsLock)

	md := findPluginModule(pluginpath)
	if md == nil {
		return "plugin not found: " + pluginpath
	}
	if md.refcount > 0 {
		return pluginDependentsMsg(md)
	}

	// Convert every reachable held descriptor pointing into this
	// module to a panic-on-call. Two distinct sources:
	//
	//   (1) Static closure descriptors emitted by the compiler for
	//       the plugin's own PFUNC values. Reachable from anything
	//       (host or other plugin) that holds a function-typed
	//       variable whose address ultimately resolves into this
	//       module's data — e.g. the host obtained `*[]func()` via
	//       Lookup-of-variable. Rewritten via canonicalSyms walk.
	//
	//   (2) Heap descriptors allocated per call by plugin.Lookup
	//       when the user requested a function symbol — each such
	//       Lookup escapes a 1-word holder containing the dlsym'd
	//       code address; the func value's data pointer points at
	//       that holder, not at the static descriptor. The plugin
	//       package threads the slice of holders to us.
	//
	// Closures created at runtime inside the plugin and returned to
	// the host are reachable from neither source; their calls after
	// Close fault fatally. See pluginClosedFuncStub's doc comment.
	//
	// Both rewrites must happen BEFORE dlclose (added in the second
	// half of v2): once the plugin's text and data are unmapped we
	// can neither read the descriptor's current contents nor write
	// new ones without faulting.
	stubPC := abi.FuncPCABIInternal(pluginClosedFuncStub)
	neutraliseStaticDescriptors(md, stubPC)
	for _, descF := range heapDescriptors {
		if descF != nil {
			*descF = stubPC
		}
	}

	// Remove canonicalSyms entries owned by this module — they
	// referenced this module's now-disappearing addresses.
	for name, entry := range canonicalSyms {
		if entry.owner == md {
			delete(canonicalSyms, name)
		}
	}

	// Remove pkgOwners entries naming this module as owner — peers
	// loaded after this point that import the same package will see
	// no owner and become canonical themselves.
	for k, owner := range pkgOwners {
		if owner == md {
			delete(pkgOwners, k)
		}
	}

	// Decrement refcount on every module THIS one was depending on.
	// We didn't track outbound edges directly; iterate to find them.
	for _, other := range activeModules() {
		if other == md {
			continue
		}
		if _, ok := other.dependents[md]; ok {
			delete(other.dependents, md)
			other.refcount--
		}
	}

	// Drop the closing module's own dependents map (if any) from the
	// GC anchor slice — this module won't appear as a target again,
	// so the map is no longer needed. Compacting in-place keeps the
	// slice O(N). Maps are not directly comparable, so we compare the
	// underlying *Map header via unsafe.Pointer.
	if md.dependents != nil {
		closing := *(*unsafe.Pointer)(unsafe.Pointer(&md.dependents))
		w := dependentsRoots[:0]
		for _, m := range dependentsRoots {
			if *(*unsafe.Pointer)(unsafe.Pointer(&m)) != closing {
				w = append(w, m)
			}
		}
		dependentsRoots = w
		md.dependents = nil
	}

	// Mark the module bad so it won't appear in subsequent
	// modulesinit results, and unlink it from the firstmoduledata
	// chain so the chain walker skips it. We don't dlclose the .so
	// in v1; the OS keeps the mapping but no Go state references it.
	md.bad = true
	detachModule(md)
	// Rebuild activeModules so the unlinked module disappears from
	// future iterations.
	modulesinit()
	return ""
}

// detachModule unlinks md from the firstmoduledata chain. Shared by
// plugin_close and rejectLoadedPlugin (the Windows version-mismatch
// path), which must both remove a module before its backing image can
// go away.
//
// If md is the chain tail, addmoduledata's append cursor still points
// at it; retreat the cursor to the new tail. Without this, the next
// module load stores through lastmoduledatap into the closed plugin's
// (about to be unmapped) image — faulting where the OS unmaps eagerly
// (darwin dlopen, windows addmoduledata) — or, where the mapping
// lingers (glibc), silently links the new module onto the detached
// node, leaving it unreachable from firstmoduledata and surfacing as
// "plugin already loaded". See issue #5.
//
// Callers hold pluginsMu (via closePlugin / open), which serialises
// chain mutation against concurrent loads.
func detachModule(md *moduledata) {
	for prev := &firstmoduledata; prev.next != nil; prev = prev.next {
		if prev.next == md {
			prev.next = md.next
			if lastmoduledatap == md {
				lastmoduledatap = prev
			}
			break
		}
	}
}

// plugin_chainTail reports the address of the last moduledata on the
// chain. plugin.Open on Windows snapshots it before LoadLibrary and
// compares after: addmoduledata (run from the plugin's DllMain inside
// LoadLibrary) appends exactly one node, so an unchanged tail means the
// DLL never registered in THIS process — typically because it was
// linked against another program's import library and PE bound its
// imports by module name elsewhere. The caller must refuse such a load
// instead of validating: plugin_validatePkghashes takes the last
// non-bad module as the just-loaded candidate, and on a chain that did
// not grow that is a healthy earlier plugin — the peer scan would
// exclude the true owner, silently passing a divergent build, or a
// host-carrier mismatch would mark and detach the WRONG moduledata
// while the offender's image is unmapped. Observed in CI while building
// the trendvidia/go#24 peer-lock fixture (trendvidia/go#31).
//
// Caller holds pluginsMu (as open does), which serialises chain
// mutation against concurrent loads and closes.
//
//go:linkname plugin_chainTail plugin.chainTail
func plugin_chainTail() uintptr {
	tail := &firstmoduledata
	for tail.next != nil {
		tail = tail.next
	}
	return uintptr(unsafe.Pointer(tail))
}

// pluginDependentsMsg formats the Close-refusal error. It names each
// dependent pluginpath so the caller can act on the refusal without a
// separate Plugin.Dependents round-trip (trendvidia/go#26).
//
// Keys with an empty pluginpath would be host-class edges. No code
// path records one today — recordDependency's referencer is always a
// loading plugin, and the host registers nothing — but if one ever
// did, name it here rather than silently omitting it from the reason
// while still refusing the Close.
func pluginDependentsMsg(md *moduledata) string {
	host := false
	for dep := range md.dependents {
		if dep.pluginpath == "" {
			host = true
		}
	}
	msg := "plugin has dependents; Close them first:"
	first := true
	// Walk activeModules rather than the map so names come out in
	// load order instead of map iteration order.
	for _, dep := range activeModules() {
		if dep.pluginpath == "" {
			continue
		}
		if _, ok := md.dependents[dep]; !ok {
			continue
		}
		if !first {
			msg += ","
		}
		first = false
		msg += " " + dep.pluginpath
	}
	if host {
		msg += "; the host program also holds references into it"
	}
	return msg
}

// plugin_syncBeforeUnmap is a stop-the-world barrier the plugin package
// takes AFTER plugin_close has unlinked a closed plugin's moduledata
// (its modulesinit above drops the module from activeModules) and
// BEFORE the plugin's image is unmapped by dlclose/FreeLibrary.
//
// The GC scans every active module's data/bss roots in runtime.markroot
// by ranging over activeModules(). A mark worker that loaded the module
// slice before plugin_close's modulesinit() can still be mid-scan of the
// closing plugin's data when the unmap frees those pages, faulting
// (observed as an access violation in markroot during Plugin.Close on
// windows/amd64, timing-dependent). Stopping and restarting the world
// forces every P past a safe point, so no worker retains the pre-unlink
// slice; after startTheWorld, markroot reloads activeModules() (the
// closed module now absent) and the subsequent unmap touches no memory
// the GC will scan.
//
//go:linkname plugin_syncBeforeUnmap plugin.syncBeforeUnmap
func plugin_syncBeforeUnmap() {
	stw := stopTheWorld(stwPluginClose)
	startTheWorld(stw)
}
