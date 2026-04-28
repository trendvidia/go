// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/stringslite"
	"unsafe"
)

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
			return "", nil, nil, "plugin was built with a different version of package " + pkghash.modulename
		}
	}

	// Initialize the freshly loaded module.
	modulesinit()
	globalptrsinit()
	funcptrsinit()
	typelinksinit()

	pluginftabverify(md)
	moduledataverify1(md)

	lock(&itabLock)
	for _, i := range md.itablinks {
		itabAdd(i)
	}
	unlock(&itabLock)

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
	// Tell runtime.exit (Windows) to use TerminateProcess instead of
	// ExitProcess once any plugin is loaded: ExitProcess's DLL_PROCESS_DETACH
	// walk deadlocks with 2+ Go-built plugin DLLs in the process.
	atomic.Store(&hostHasLoadedPlugins, 1)
	return md.pluginpath, syms, md.inittasks, ""
}

// hostHasLoadedPlugins is set to 1 once a plugin is successfully loaded
// into the host. Read by runtime.exit on Windows.
var hostHasLoadedPlugins uint32

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
	}
	return false
}

// skipHostSharedInits sets state = 2 (Done) on plugin-local inittasks
// for any package pluginSkipInitPkg flags, so doInit1 short-circuits
// them. Safe to call unconditionally — for packages outside the skip
// set or when the inittask has already been deduped to state=2 by the
// platform dynamic loader (linux/darwin), this is a no-op.
func skipHostSharedInits(md *moduledata) {
	for _, t := range md.inittasks {
		if t == nil || t.state == 2 || t.nfns == 0 {
			continue
		}
		firstFunc := add(unsafe.Pointer(t), 8)
		f := *(*func())(unsafe.Pointer(&firstFunc))
		fi := findfunc(abi.FuncPCABIInternal(f))
		name := funcname(fi)
		pkg := pkgFromInitName(name)
		if pluginSkipInitPkg(pkg) {
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
		return "plugin has dependents; Close them first"
	}

	// Convert every held func value pointing into this module to a
	// panic-on-call. Two distinct sources:
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

	// Mark the module bad so it won't appear in subsequent
	// modulesinit results, and unlink it from the firstmoduledata
	// chain so the chain walker skips it. We don't dlclose the .so
	// in v1; the OS keeps the mapping but no Go state references it.
	md.bad = true
	for prev := &firstmoduledata; prev.next != nil; prev = prev.next {
		if prev.next == md {
			prev.next = md.next
			break
		}
	}
	// Rebuild activeModules so the unlinked module disappears from
	// future iterations.
	modulesinit()
	return ""
}
