// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bufio"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cmd/internal/objabi"
	"cmd/internal/sys"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
)

// peSkipSymName reports whether the given symbol name should be excluded
// from PE export tables and import libraries. It filters out internal
// linker symbols, PE artifacts, and type descriptors.
func peSkipSymName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, "__") {
		return true
	}
	if strings.HasPrefix(name, "type:") || strings.HasPrefix(name, "type..") {
		return true
	}
	// Compiler-auto-generated per-package symbols use a `pkg..symname.N`
	// pattern (double dot in the middle): interface-switch caches,
	// type-assert caches, gcprog tables, etc. These are layout-specific
	// to the binary that generated them — host's and plugin's copies
	// have unrelated contents. Host-sharing them would convert the
	// plugin's local copy to SHOSTOBJ and route accesses to host's
	// unrelated content, breaking the cache semantics. Keep local.
	if i := strings.Index(name, ".."); i > 0 && i < len(name)-2 {
		return true
	}
	return false
}

// peSectionMarkerSym reports whether name is a linker-defined section
// boundary symbol. These mark the start/end of PE sections in the
// moduledata and must never be imported from the host — doing so would
// make the plugin's moduledata point to the HOST's section bounds
// instead of the DLL's, causing the GC to crash.
var peSectionMarkers = map[string]bool{
	"runtime.text":       true,
	"runtime.etext":      true,
	"runtime.types":      true,
	"runtime.etypes":     true,
	"runtime.rodata":     true,
	"runtime.erodata":    true,
	"runtime.noptrdata":  true,
	"runtime.enoptrdata": true,
	"runtime.data":       true,
	"runtime.edata":      true,
	"runtime.bss":        true,
	"runtime.ebss":       true,
	"runtime.noptrbss":   true,
	"runtime.enoptrbss":  true,
	"runtime.end":        true,
	"runtime.gcdata":     true,
	"runtime.gcbss":      true,
	"runtime.covctrs":    true,
	"runtime.ecovctrs":   true,
}

// peSharedRuntimeData lists runtime DATA singletons that must resolve to
// host's instance, not a plugin-local copy. Plugin-local text bodies that
// dereference these globals would otherwise see uninitialized state and
// crash on the first heap allocation / goroutine spawn / GC cycle.
//
// The compiler emits R_GOTPCREL for every extern data access under
// -dynlink; peGotRedirect retargets those to a `__imp_X` slot. For names
// in this map, peLoadImportLibrary creates a SHOSTOBJ `__imp_X` that the
// OS loader fills with host's address. For names NOT in this map,
// peGotRedirect synthesizes a plugin-local SNOPTRDATA `__imp_X` holding
// &X — the plugin reads its own (uninitialized) copy.
//
// What belongs here: process-wide runtime singletons whose state must be
// consistent across host and plugin (allocator, scheduler, goroutine
// table, GC state, finalizer queue, profiler buckets, netpoll, module
// list, itab cache, panic/exit lifecycle).
//
// What stays local: per-binary identity bits (iscgo, islibrary, isarchive,
// mainStarted), section boundary markers (covered by peSectionMarkers),
// and per-binary type descriptors / closure captures.
var peSharedRuntimeData = map[string]bool{
	// Memory allocator + page management.
	"runtime.mheap_":          true,
	"runtime.memstats":        true,
	"runtime.MemProfileRate":  true,
	"runtime.physPageSize":    true,
	"runtime.physHugePageSize": true,
	"runtime.persistentChunks": true,
	"runtime.globalAlloc":     true,
	"runtime.gcBitsArenas":    true,
	"runtime.scavenger":       true,
	"runtime.scavenge":        true,
	"runtime.zerobase":        true,

	// Sentinel mspan stored in mcache.alloc[spc] when no span is cached.
	// Plugin's mcache.refill compares `c.alloc[spc] != &emptymspan`. When
	// host's releaseAll (called from prepareForSweep at GC start) clears
	// the mcache slot to host's &emptymspan, plugin's compare against
	// plugin-local &emptymspan would be unequal — falling through to the
	// sweepgen check on a zeroed dummy span and throwing "bad sweepgen
	// in refill". Sharing aligns the sentinel pointer across modules.
	"runtime.emptymspan":      true,

	// Scheduler.
	"runtime.sched":               true,
	"runtime.allp":                true,
	"runtime.allpLock":            true,
	"runtime.allm":                true,
	"runtime.allocmLock":          true,
	"runtime.execLock":            true,
	"runtime.idlepMask":           true,
	"runtime.timerpMask":          true,
	"runtime.gomaxprocs":          true,
	"runtime.newprocs":            true,
	"runtime.starttime":           true,
	"runtime.newmHandoff":         true,
	"runtime.stealOrder":          true,
	"runtime.computeMaxProcsLock": true,
	"runtime.numCPUStartup":       true,

	// Goroutine table.
	"runtime.allgs":     true,
	"runtime.allgptr":   true,
	"runtime.allglen":   true,
	"runtime.allglock":  true,

	// GC state.
	"runtime.gcController":          true,
	"runtime.gcphase":               true,
	"runtime.gcsema":                true,
	"runtime.gcBlackenEnabled":      true,
	"runtime.gcCPULimiter":          true,
	"runtime.work":                  true,
	"runtime.worldsema":             true,
	"runtime.casgstatusAlwaysTrack": true,
	"runtime.useCheckmark":          true,
	"runtime.sweep":                 true,
	"runtime.gcBgMarkWorkerCount":   true,
	"runtime.gcBgMarkWorkerPool":    true,
	"runtime.gcCleanups":            true,
	"runtime.gcMarkDoneFlushed":     true,
	"runtime.gcDebugMarkDone":       true,

	// Lifecycle.
	"runtime.forcegc":       true,
	"runtime.forcegcperiod": true,
	"runtime.freezing":      true,
	"runtime.exiting":       true,
	"runtime.panicking":     true,
	"runtime.paniclk":       true,
	"runtime.runtimeInitTime": true,
	"runtime.ticks":         true,

	// Finalizer queue.
	"runtime.allfin":      true,
	"runtime.finalizer1":  true,
	"runtime.finc":        true,
	"runtime.fing":        true,
	"runtime.fingStatus":  true,
	"runtime.finlock":     true,
	"runtime.finptrmask":  true,
	"runtime.finq":        true,
	"runtime.finqueued":   true,
	"runtime.userArenaState": true,

	// Module list (paired with lastmoduledatap above). Note:
	// runtime.firstmoduledata can't go here — mingw ld's
	// --export-all-symbols skips it (size=0 in the partial PE; the
	// linker fills bytes via symtab.go without updating the symbol
	// size), so the OS loader can't resolve a plugin import for it.
	// We instead route plugin's findfunc / module-list walks into
	// host's bodies via the broad TEXT conversion below.
	"runtime.modulesSlice": true,

	// Itab cache (process-wide).
	"runtime.itabLock":  true,
	"runtime.itabTable": true,

	// Profiler state.
	"runtime.mProfCycle":         true,
	"runtime.mbuckets":           true,
	"runtime.xbuckets":           true,
	"runtime.bbuckets":           true,
	"runtime.buckhash":           true,
	"runtime.profBlockLock":      true,
	"runtime.profInsertLock":     true,
	"runtime.profMemActiveLock":  true,
	"runtime.profMemFutureLock":  true,
	"runtime.blockprofilerate":   true,
	"runtime.mutexprofilerate":   true,
	"runtime.profiletimer":       true,
	"runtime.cpuprof":            true,
	"runtime.runningPanicDefers": true,

	// Netpoll (Windows).
	"runtime.iocphandle":       true,
	"runtime.netpollInited":    true,
	"runtime.netpollInitLock":  true,
	"runtime.netpollWaiters":   true,
	"runtime.netpollWakeSig":   true,
	"runtime.isIdleInSynctest": true,

	// Windows syscall trampoline state. asmstdcallAddr is a function
	// pointer set by host's runtime.osinit to the address of the
	// asmstdcall trampoline; runtime.stdcall passes it as `fn` to
	// asmcgocall. Plugin's runtime.osinit doesn't run, so plugin's
	// local copy is NULL — leading to `asmcgocall(NULL, ...)` →
	// landingpad CALL AX with AX=0 fault on first VirtualAlloc/etc.
	"runtime.asmstdcallAddr":   true,
	"runtime.asyncPreemptStack": true,

	// Trace ring.
	"runtime.trace":             true,
	"runtime.traceAdvancer":     true,
	"runtime.traceAdvanceSema":  true,
	"runtime.traceShutdownSema": true,
	"runtime.traceback_cache":   true,

	// Misc process-wide state.
	"runtime.hashkey":                    true,
	"runtime.globalRand":                 true,
	"runtime.heapRandSeed":               true,
	"runtime.heapRandSeedBitsRemaining":  true,
	"runtime.crashFD":                    true,
	"runtime.faketime":                   true,
	"runtime.haveHighResTimer":           true,
	"runtime.semtable":                   true,
	"runtime.poolcleanup":                true,
	"runtime.cgoHasExtraM":               true,
	"runtime.extraMInUse":                true,
	"runtime.extraMLength":               true,
	"runtime.canonicalSyms":              true,
	"runtime.deadlock":                   true,
	"runtime.printBacklog":               true,
	"runtime.printBacklogIndex":          true,
	"runtime.suspendLock":                true,
	"runtime.isWaitingForSuspendG":       true,
	"runtime.didothers":                  true,
	"runtime.boringCaches":               true,
	"runtime.reflectOffs":                true,

	// User-package globals shared from the host binary. Pairs with
	// runtime.pluginSkipInitPkgs in src/runtime/plugin.go: when a
	// user package's data is host-shared, plugin's init for that
	// package must be short-circuited so it doesn't clobber host's
	// already-initialized state. Currently testplugin-only.
	"testplugin/common.X": true,
}

// peSharedV1Funcs lists package-level (non-method) functions whose v=1
// ABIInternal entry must be host-bound so that FuncPCABIInternal returns
// host's PC and the plugin's runtime body routes through host's body.
// Required for runtime.init.5 to advance past funcMaxSPDelta nil-datap.
//
// What belongs here: pure-Go runtime functions referenced by FuncPCABIInternal
// from any package init, plus runtime introspection helpers (findfunc,
// funcMaxSPDelta) that must walk host's moduledata to resolve cross-binary
// PCs.
//
// What stays out: method bodies (anything matching `pkg.(*Recv).Method`) —
// converting their v=1 to SHOSTOBJ orphans type-descriptor R_METHODOFF /
// R_ADDROFF relocs at "missing section for relocation target ..." in
// data.go:relocsym.
var peSharedV1Funcs = map[string]bool{
	"runtime.findfunc":       true,
	"runtime.funcMaxSPDelta": true,
	"runtime.asyncPreempt2":  true,
	"runtime.xRegRestore":    true,
}

// peGotRedirect returns the symbol that an R_GOTPCREL relocation targeting
// `targ` should be retargeted to on PE. The compiler's -dynlink rewrite
// emits `MOVQ X@GOT, R15; MOVx (R15), Y` for every extern data access; on
// PE we resolve "@GOT" through a `__imp_X`-named slot that holds X's
// address.
//
// Two cases:
//
//  1. Host-shared: peLoadImportLibrary already created `__imp_X` as
//     SHOSTOBJ for symbols imported from the host. mingw ld resolves the
//     SHOSTOBJ ref through its BFD long-format import library to a real
//     PE Import Directory entry; the OS loader fills the IAT slot at
//     load time with the host's address of X. The Go linker just needs
//     to emit the COFF reloc against `__imp_X`.
//
//  2. Plugin-local: there is no IAT slot for plugin-defined symbols.
//     Synthesize an SDATA `__imp_X` containing &X (R_ADDR), 8 bytes.
//     mingw ld emits IMAGE_REL_AMD64_ADDR64 → PE base reloc; the OS
//     loader rebases at load time.
//
// `__imp_X` SHOSTOBJ and `__imp_X` SDATA cannot coexist for the same X,
// but X is either host-shared or plugin-local — mutually exclusive.
//
// Only call on Windows under -dynlink. Other heads/modes have their own
// GOT machinery (ELF/Darwin via lib.go:AddGotSym).
func peGotRedirect(arch *sys.Arch, ldr *loader.Loader, targ loader.Sym) loader.Sym {
	// For host-shared symbols (SHOSTOBJ), peLoadImportLibrary created
	// `__imp_<host-export-name>` as the SHOSTOBJ companion — keyed on the
	// MANGLED export name (e.g. `__imp_runtime.findfunc.abi0`), not the
	// plugin-internal name (`runtime.findfunc` v=0). Match that key so
	// the GOT redirect lands on the OS-loader-populated IAT slot instead
	// of synthesizing a plugin-local SDATA pointer to the plugin's own
	// JMP thunk — which would orphan host-shared functions like
	// `runtime.findfunc` whose callers receive PCs that the host's
	// findfunc cannot resolve.
	internalName := ldr.SymName(targ)
	name := "__imp_" + internalName
	if ldr.SymType(targ) == sym.SHOSTOBJ {
		if ext := ldr.SymExtname(targ); ext != "" && ext != internalName {
			name = "__imp_" + ext
		}
	}
	if s := ldr.Lookup(name, 0); s != 0 {
		st := ldr.SymType(s)
		if st == sym.SHOSTOBJ || st == sym.SNOPTRDATA {
			ldr.SetAttrReachable(s, true)
			return s
		}
	}
	s := ldr.LookupOrCreateSym(name, 0)
	sb := ldr.MakeSymbolUpdater(s)
	// SNOPTRDATA: the slot holds a pointer but it's an IAT-style view
	// into the host image / OS-loader-rebased address — never a Go heap
	// pointer the GC should trace. Putting it in noptrdata also keeps
	// the gcdata pass quiet (it errors on SDATA without Go type info).
	sb.SetType(sym.SNOPTRDATA)
	sb.SetSize(0)
	sb.SetData(nil)
	sb.AddAddrPlus(arch, targ, 0)
	ldr.SetAttrReachable(s, true)
	return s
}

// peRetargetGotRelocs walks every reachable Textp symbol on Windows and
// retargets R_GOTPCREL relocs to a `__imp_X`-named slot via peGotRedirect.
// Runs after Gentext (so all reachable text is known) and before dodata
// (so synthesized SDATA `__imp_X` symbols get section assignment +
// SymDynid via writeSymbols).
//
// Without this, extreloc would synthesize the slots lazily at reloc-emit
// time — too late for writeSymbols to give them COFF symbol-table entries.
//
// The compiler emits R_GOTPCREL only under -dynlink, which cmd/go now
// passes for Windows plugin builds. Pre-dynlink builds have no R_GOTPCREL
// relocs and the pass is a no-op.
func peRetargetGotRelocs(ctxt *Link) {
	if !ctxt.IsWindows() || !ctxt.IsAMD64() {
		return
	}
	ldr := ctxt.loader
	walk := func(s loader.Sym) {
		relocs := ldr.Relocs(s)
		var sb *loader.SymbolBuilder
		for ri := 0; ri < relocs.Count(); ri++ {
			r := relocs.At(ri)
			if r.Type() != objabi.R_GOTPCREL {
				continue
			}
			rs := r.Sym()
			if rs == 0 {
				continue
			}
			redirect := peGotRedirect(ctxt.Arch, ldr, rs)
			if redirect == rs {
				continue
			}
			if sb == nil {
				sb = ldr.MakeSymbolUpdater(s)
			}
			sb.SetRelocSym(ri, redirect)
		}
	}
	for _, s := range ctxt.Textp {
		walk(s)
	}
}

// impLibSymbol holds info about a symbol to include in the import library.
type impLibSymbol struct {
	name   string
	isFunc bool
}

// peGenerateImportLibrary creates a MinGW-compatible import library (.a)
// for the current executable. This allows plugin DLLs to link against the
// host's exported symbols without requiring an external linker (cgo) for
// the host build.
//
// On amd64 the archive uses BFD long format: per-symbol stub objects with
// a .text thunk plus .idata$2/$4/$5/$6/$7 contributions, anchored by a
// head and tail object. This is the format mingw ld extracts Import
// Directory entries from (mingw ld 2.46 does NOT generate Import Directory
// entries from IMPORT_OBJECT_HEADER short-import archives; it auto-imports
// via 32-bit pseudo-relocs that overflow on 64-bit Windows).
//
// On i386/arm64 the archive falls back to the IMPORT_OBJECT_HEADER short
// format. peLoadImportLibrary (the internal-link path) only consumes the
// "/" symbol index, so internal-link plugins work on every arch regardless
// of which member format the archive contains. External-link plugins on
// non-amd64 are out of scope here.
//
// This function collects exportable symbols directly from the loader,
// independent of the dexport slice used by the PE export table.
func peGenerateImportLibrary(ctxt *Link) {
	implib := *flagOutfile + ".a"
	// Use the final executable name (not the temp build path) for the
	// DLL name in the import library. This ensures the plugin DLL
	// imports from "host.exe" instead of "a.out.exe".
	dllName := filepath.Base(*flagOutfile)
	if *flagPluginExeName != "" {
		dllName = *flagPluginExeName
	}

	var machine uint16
	switch ctxt.Arch.Family {
	case sys.AMD64:
		machine = pe.IMAGE_FILE_MACHINE_AMD64
	case sys.I386:
		machine = pe.IMAGE_FILE_MACHINE_I386
	case sys.ARM64:
		machine = pe.IMAGE_FILE_MACHINE_ARM64
	default:
		Errorf("unsupported architecture for import library generation")
		return
	}

	ldr := ctxt.loader

	// Collect exportable symbols from the loader. Only include reachable
	// symbols — the host's PE export table only contains reachable symbols,
	// so the import library must match. Critical symbols like
	// runtime.addmoduledata are made reachable by the deadcode pass
	// (which marks all defined symbols reachable for plugin hosts).
	//
	// Names go through mangleABIName so a function with both ABI variants
	// produces two distinct entries: "X" (the ABIInternal entry on a
	// non-shared host) and "X.abi0" (the ABI0 body). External-link
	// plugins need the ABI0 body separately because compiler-emitted
	// call sites for runtime helpers like `runtime.systemstack` use the
	// ABI0 stack-arg shape and cannot tolerate the host's ABIInternal
	// wrapper, whose `mov %rax,(%rsp)` clobbers the caller's stack arg.
	seen := make(map[string]bool)
	var symbols []impLibSymbol
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		st := ldr.SymType(s)
		if !st.IsText() && !st.IsRODATA() && !st.IsData() {
			continue
		}
		name := mangleABIName(ctxt, ldr, s, ldr.SymExtname(s))
		if peSkipSymName(name) || seen[name] {
			continue
		}
		seen[name] = true
		isFunc := st.IsText()
		symbols = append(symbols, impLibSymbol{name: name, isFunc: isFunc})
		// PE export ordinals are uint16; cap at 65535.
		if len(symbols) >= 0xFFFF {
			break
		}
	}

	if len(symbols) == 0 {
		return
	}

	// Sort alphabetically (required by PE import library convention).
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].name < symbols[j].name
	})

	f, err := os.Create(implib)
	if err != nil {
		Exitf("cannot create import library %s: %v", implib, err)
	}

	w := bufio.NewWriter(f)
	w.WriteString("!<arch>\n")

	if machine == pe.IMAGE_FILE_MACHINE_AMD64 {
		writeBfdArchive(w, symbols, dllName, machine)
	} else {
		// Non-amd64 fallback: short-import format. Internal-link
		// plugins still work because peLoadImportLibrary only reads
		// the "/" symbol index. External-link plugins on these arches
		// are not exercised today.
		writeFirstLinkerMember(w, symbols, dllName, machine)
		for i, sym := range symbols {
			writeShortImport(w, sym.name, dllName, machine, sym.isFunc, uint16(i))
		}
	}

	if err := w.Flush(); err != nil {
		Exitf("error writing import library %s: %v", implib, err)
	}
	if err := f.Close(); err != nil {
		Exitf("error closing import library %s: %v", implib, err)
	}
}

// writeFirstLinkerMember writes the "/" archive member that serves as the
// symbol directory for the import library.
func writeFirstLinkerMember(f io.Writer, symbols []impLibSymbol, dllName string, machine uint16) {
	numSyms := len(symbols)

	// Calculate the offset where each import object will start.
	// Size of linker member data: 4 + numSyms*4 + sum(len(name)+1)
	var namesSize int
	for _, sym := range symbols {
		namesSize += len(sym.name) + 1
	}
	linkerDataSize := 4 + numSyms*4 + namesSize

	// Pad to even boundary if needed for ar header calculation.
	linkerMemberSize := linkerDataSize
	linkerPad := linkerMemberSize % 2

	// The first import object starts after: ar_magic(8) + linkerMember_arHeader(60) + linkerData + pad
	firstObjOffset := 8 + 60 + linkerMemberSize + linkerPad

	// Calculate offset for each import object.
	offsets := make([]uint32, numSyms)
	currentOffset := firstObjOffset
	for i, sym := range symbols {
		offsets[i] = uint32(currentOffset)
		objSize := shortImportSize(sym.name, dllName)
		currentOffset += 60 + objSize // 60 = ar header size
		if objSize%2 != 0 {
			currentOffset++ // ar padding
		}
	}

	// Write ar header for linker member.
	writeArHeader(f, "/", linkerMemberSize)

	// Write number of symbols (big-endian).
	binary.Write(f, binary.BigEndian, uint32(numSyms))

	// Write offsets (big-endian).
	for _, off := range offsets {
		binary.Write(f, binary.BigEndian, off)
	}

	// Write symbol names (null-terminated).
	for _, sym := range symbols {
		io.WriteString(f, sym.name)
		f.Write([]byte{0})
	}

	// Pad to even boundary.
	if linkerPad != 0 {
		f.Write([]byte{'\n'})
	}
}

// shortImportSize returns the data size of a short import object.
func shortImportSize(symName, dllName string) int {
	// Short import header (20 bytes) + symbol name + \0 + dll name + \0
	return 20 + len(symName) + 1 + len(dllName) + 1
}

// writeShortImport writes one short import object as an ar member.
// This uses the IMPORT_OBJECT_HEADER format from the PE/COFF spec.
func writeShortImport(f io.Writer, symName, dllName string, machine uint16, isFunc bool, hint uint16) {
	dataSize := len(symName) + 1 + len(dllName) + 1
	totalSize := 20 + dataSize // 20 = sizeof(IMPORT_OBJECT_HEADER)

	// Use symbol name as ar member name, truncated to 15 chars.
	arName := symName
	if len(arName) > 15 {
		arName = arName[:15]
	}

	writeArHeader(f, arName, totalSize)

	// IMPORT_OBJECT_HEADER (20 bytes):
	var hdr [20]byte
	// Sig1 = 0x0000 (IMAGE_FILE_MACHINE_UNKNOWN)
	binary.LittleEndian.PutUint16(hdr[0:2], 0x0000)
	// Sig2 = 0xFFFF
	binary.LittleEndian.PutUint16(hdr[2:4], 0xFFFF)
	// Version = 0
	binary.LittleEndian.PutUint16(hdr[4:6], 0)
	// Machine
	binary.LittleEndian.PutUint16(hdr[6:8], machine)
	// TimeDateStamp = 0
	binary.LittleEndian.PutUint32(hdr[8:12], 0)
	// SizeOfData = size of strings after header
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(dataSize))
	// OrdinalHint
	binary.LittleEndian.PutUint16(hdr[16:18], hint)
	// Type (2 bits) | NameType (3 bits) | Reserved (11 bits)
	// Type: IMPORT_OBJECT_CODE=0, IMPORT_OBJECT_DATA=1
	// NameType: IMPORT_OBJECT_NAME=1 (name matches symbol)
	var typeField uint16
	if isFunc {
		typeField = 0 // IMPORT_OBJECT_CODE
	} else {
		typeField = 1 // IMPORT_OBJECT_DATA
	}
	typeField |= (1 << 2) // NameType = IMPORT_OBJECT_NAME (bits 2-4)
	binary.LittleEndian.PutUint16(hdr[18:20], typeField)

	f.Write(hdr[:])

	// Symbol name (null-terminated)
	io.WriteString(f, symName)
	f.Write([]byte{0})

	// DLL name (null-terminated)
	io.WriteString(f, dllName)
	f.Write([]byte{0})

	// Pad to even boundary.
	if totalSize%2 != 0 {
		f.Write([]byte{'\n'})
	}
}

// writeArHeader writes a 60-byte ar archive member header.
func writeArHeader(f io.Writer, name string, size int) {
	// Format: name/16 + mtime/12 + uid/6 + gid/6 + mode/8 + size/10 + magic/2
	var hdr [60]byte

	// Name field (16 bytes, space-padded). Per ar(5), "/" and "//" are
	// special names (symbol index and extended-name table); they are
	// emitted verbatim without the trailing "/" suffix. Regular member
	// names get the trailing "/" used as a name terminator.
	arName := name
	if name != "/" && name != "//" {
		arName = name + "/"
	}
	copy(hdr[0:16], fmt.Sprintf("%-16s", arName))
	// Timestamp (12 bytes).
	copy(hdr[16:28], "0           ")
	// UID (6 bytes).
	copy(hdr[28:34], "0     ")
	// GID (6 bytes).
	copy(hdr[34:40], "0     ")
	// Mode (8 bytes).
	copy(hdr[40:48], "100666  ")
	// Size (10 bytes).
	copy(hdr[48:58], fmt.Sprintf("%-10d", size))
	// Magic.
	hdr[58] = '`'
	hdr[59] = '\n'

	f.Write(hdr[:])
}

// BFD long-format import library writer (amd64 only).
//
// Layout per archive (ar):
//
//	"/"          — System V symbol index. Maps each public symbol X
//	               (and its companion __imp_X) to its stub member, plus
//	               two helpers: the head-anchor "_head_<dll>_a" and the
//	               iname-anchor "__<dll>_a_iname".
//	"h.o"        — head COFF object: defines _head_<dll>_a and the 20-byte
//	               IMAGE_IMPORT_DESCRIPTOR in .idata$2 with three ADDR32NB
//	               relocs (OriginalFirstThunk → .idata$4, Name → __iname,
//	               FirstThunk → .idata$5).
//	"sNNNNN.o"   — one stub per symbol. .text holds an 8-byte indirect-jmp
//	               thunk (ff 25 disp32 / nop / nop) with REL32 reloc to
//	               __imp_X; .idata$4/$5 each hold an 8-byte ILT/IAT slot
//	               with ADDR32NB reloc to .idata$6 (hint+name); .idata$6
//	               holds the 2-byte hint and the imported symbol name;
//	               .idata$7 holds a 4-byte ADDR32NB back-ref to the head
//	               (DLL grouping marker).
//	"t.o"        — tail COFF object: defines __<dll>_a_iname pointing at
//	               the DLL-name string in .idata$7, plus 8-byte NULL
//	               terminators in .idata$4/$5.
//
// After link, the .idata$N suffixes coalesce to: $2 (per-DLL Import
// Descriptors), $4 (ILT), $5 (IAT), $6 (hint/name table), $7 (DLL name
// strings). Within each suffix, the head sorts before stubs and the tail
// sorts after, so OriginalFirstThunk/FirstThunk point at the start of
// this DLL's slot run and the NULL terminators close it.
//
// Reference: dlltool's gen_lib_file in binutils/dlltool.c, and the layout
// of mingw's own libkernel32.a.

const (
	coffSymClassExternal = 2
	coffSymClassStatic   = 3

	coffRelocAMD64ADDR32NB = 0x0003
	coffRelocAMD64REL32    = 0x0004

	// Section characteristics.
	coffCharText  = 0x60300020 // CODE | EXEC | READ | ALIGN_4
	coffCharIData = 0xC0300040 // INITIALIZED_DATA | READ | WRITE | ALIGN_4
	coffCharHint  = 0xC0200040 // INITIALIZED_DATA | READ | WRITE | ALIGN_2
)

type coffReloc struct {
	offset uint32
	symIdx uint32
	typ    uint16
}

type coffSection struct {
	name   string
	data   []byte
	relocs []coffReloc
	chars  uint32
}

type coffSymbol struct {
	name    string
	value   uint32
	section int16 // 1-based; 0 = undefined
	typ     uint16
	class   uint8
}

// writeCoffObject emits a single COFF object file (one ar member) given
// section and symbol definitions. Names longer than 8 bytes go into the
// string table.
func writeCoffObject(w io.Writer, machine uint16, sections []coffSection, syms []coffSymbol) {
	var strtab bytes.Buffer
	strtab.Write([]byte{0, 0, 0, 0}) // size placeholder, filled at the end.

	// nameBytes returns the 8-byte name field, using the string table for
	// names > 8 chars.
	nameBytes := func(n string) [8]byte {
		var b [8]byte
		if len(n) <= 8 {
			copy(b[:], n)
			return b
		}
		off := strtab.Len()
		strtab.WriteString(n)
		strtab.WriteByte(0)
		// (zero, offset) encoding.
		binary.LittleEndian.PutUint32(b[:4], 0)
		binary.LittleEndian.PutUint32(b[4:], uint32(off))
		return b
	}

	nsec := len(sections)
	pos := uint32(20 + 40*nsec)
	type sectInfo struct{ rawOff, relocOff uint32 }
	info := make([]sectInfo, nsec)
	for i, s := range sections {
		if len(s.data) > 0 {
			info[i].rawOff = pos
			pos += uint32(len(s.data))
		}
		if len(s.relocs) > 0 {
			info[i].relocOff = pos
			pos += 10 * uint32(len(s.relocs))
		}
	}
	symPtr := pos

	// COFF file header (20 bytes).
	var fh [20]byte
	binary.LittleEndian.PutUint16(fh[0:2], machine)
	binary.LittleEndian.PutUint16(fh[2:4], uint16(nsec))
	binary.LittleEndian.PutUint32(fh[8:12], symPtr)
	binary.LittleEndian.PutUint32(fh[12:16], uint32(len(syms)))
	w.Write(fh[:])

	// Section headers.
	for i, s := range sections {
		var sh [40]byte
		nb := nameBytes(s.name)
		copy(sh[0:8], nb[:])
		binary.LittleEndian.PutUint32(sh[16:20], uint32(len(s.data)))
		binary.LittleEndian.PutUint32(sh[20:24], info[i].rawOff)
		binary.LittleEndian.PutUint32(sh[24:28], info[i].relocOff)
		binary.LittleEndian.PutUint16(sh[32:34], uint16(len(s.relocs)))
		binary.LittleEndian.PutUint32(sh[36:40], s.chars)
		w.Write(sh[:])
	}

	// Section data + relocs.
	for _, s := range sections {
		if len(s.data) > 0 {
			w.Write(s.data)
		}
		for _, r := range s.relocs {
			var rb [10]byte
			binary.LittleEndian.PutUint32(rb[0:4], r.offset)
			binary.LittleEndian.PutUint32(rb[4:8], r.symIdx)
			binary.LittleEndian.PutUint16(rb[8:10], r.typ)
			w.Write(rb[:])
		}
	}

	// Symbol table.
	for _, s := range syms {
		var sb [18]byte
		nb := nameBytes(s.name)
		copy(sb[0:8], nb[:])
		binary.LittleEndian.PutUint32(sb[8:12], s.value)
		binary.LittleEndian.PutUint16(sb[12:14], uint16(s.section))
		binary.LittleEndian.PutUint16(sb[14:16], s.typ)
		sb[16] = s.class
		sb[17] = 0
		w.Write(sb[:])
	}

	// String table (size header + bytes).
	binary.LittleEndian.PutUint32(strtab.Bytes()[0:4], uint32(strtab.Len()))
	w.Write(strtab.Bytes())
}

// bfdMunge replaces non-alphanumeric characters in s with '_' so the
// result is a valid C identifier (matches dlltool convention).
func bfdMunge(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func bfdHeadSym(dllName string) string  { return "_head_" + bfdMunge(dllName) + "_a" }
func bfdInameSym(dllName string) string { return "__" + bfdMunge(dllName) + "_a_iname" }

// writeBfdHead emits the head COFF object for the BFD long-format archive.
func writeBfdHead(w io.Writer, machine uint16, dllName string) {
	headSym := bfdHeadSym(dllName)
	inameSym := bfdInameSym(dllName)

	// Section indices (1-based for COFF):
	//   1 = .idata$2  (Import Descriptor, 20 bytes)
	//   2 = .idata$5  (anchor for FirstThunk; empty)
	//   3 = .idata$4  (anchor for OriginalFirstThunk; empty)
	//
	// Symbol indices (0-based for relocs):
	//   0 = .idata$2
	//   1 = .idata$5
	//   2 = .idata$4
	//   3 = _head_<dll>_a (defined at .idata$2 offset 0)
	//   4 = __<dll>_a_iname (undefined; defined in tail object)

	sections := []coffSection{
		{name: ".idata$2", data: make([]byte, 20), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 2, typ: coffRelocAMD64ADDR32NB},  // OriginalFirstThunk → .idata$4
			{offset: 12, symIdx: 4, typ: coffRelocAMD64ADDR32NB}, // Name → __<dll>_a_iname
			{offset: 16, symIdx: 1, typ: coffRelocAMD64ADDR32NB}, // FirstThunk → .idata$5
		}},
		{name: ".idata$5", chars: coffCharIData},
		{name: ".idata$4", chars: coffCharIData},
	}
	syms := []coffSymbol{
		{name: ".idata$2", section: 1, class: coffSymClassStatic},
		{name: ".idata$5", section: 2, class: coffSymClassStatic},
		{name: ".idata$4", section: 3, class: coffSymClassStatic},
		{name: headSym, section: 1, class: coffSymClassExternal},
		{name: inameSym, section: 0, class: coffSymClassExternal},
	}
	writeCoffObject(w, machine, sections, syms)
}

// writeBfdTail emits the tail COFF object for the BFD long-format archive.
func writeBfdTail(w io.Writer, machine uint16, dllName string) {
	inameSym := bfdInameSym(dllName)

	// .idata$7 holds the DLL name string. Pad to even length so following
	// content stays 2-byte aligned.
	dllStr := append([]byte(dllName), 0)
	if len(dllStr)%2 != 0 {
		dllStr = append(dllStr, 0)
	}

	// Section indices:
	//   1 = .idata$4 (8-byte NULL ILT terminator)
	//   2 = .idata$5 (8-byte NULL IAT terminator)
	//   3 = .idata$7 (DLL name string)
	sections := []coffSection{
		{name: ".idata$4", data: make([]byte, 8), chars: coffCharIData},
		{name: ".idata$5", data: make([]byte, 8), chars: coffCharIData},
		{name: ".idata$7", data: dllStr, chars: coffCharIData},
	}
	syms := []coffSymbol{
		{name: ".idata$4", section: 1, class: coffSymClassStatic},
		{name: ".idata$5", section: 2, class: coffSymClassStatic},
		{name: ".idata$7", section: 3, class: coffSymClassStatic},
		{name: inameSym, section: 3, class: coffSymClassExternal},
	}
	writeCoffObject(w, machine, sections, syms)
}

// writeBfdStub emits one per-symbol stub COFF object. For functions
// (isFunc=true) the stub defines a `.text` thunk `jmp *__imp_X` and
// publishes `X` at that thunk address — that's how callers reach the
// host. For data (isFunc=false) the stub only publishes `__imp_X`
// (the IAT slot) and omits the thunk; emitting a thunk that also
// publishes `X` would multiply-define `X` against the plugin's own
// data definition (Bug 8).
func writeBfdStub(w io.Writer, machine uint16, dllName, symName string, hint uint16, isFunc bool) {
	if !isFunc {
		writeBfdDataStub(w, machine, dllName, symName, hint)
		return
	}
	headSym := bfdHeadSym(dllName)
	impSym := "__imp_" + symName

	// .text: jmp *0x0(%rip); nop; nop  (8 bytes)
	// The REL32 reloc at offset 2 patches disp32 to point at __imp_X.
	text := []byte{0xff, 0x25, 0, 0, 0, 0, 0x90, 0x90}

	// .idata$6: 2-byte hint + name + null + 2-byte align pad.
	var hb bytes.Buffer
	var hintBytes [2]byte
	binary.LittleEndian.PutUint16(hintBytes[:], hint)
	hb.Write(hintBytes[:])
	hb.WriteString(symName)
	hb.WriteByte(0)
	if hb.Len()%2 != 0 {
		hb.WriteByte(0)
	}

	// Section indices:
	//   1 = .text       (thunk)
	//   2 = .idata$7    (4-byte ADDR32NB → _head_<dll>_a)
	//   3 = .idata$5    (8-byte IAT slot, ADDR32NB → .idata$6)
	//   4 = .idata$4    (8-byte ILT slot, ADDR32NB → .idata$6)
	//   5 = .idata$6    (hint + name)
	//
	// Symbol indices:
	//   0 = .text       (STATIC section sym)
	//   1 = .idata$7    (STATIC section sym)
	//   2 = .idata$5    (STATIC section sym)
	//   3 = .idata$4    (STATIC section sym)
	//   4 = .idata$6    (STATIC section sym)
	//   5 = X           (EXTERNAL, defined in .text)
	//   6 = __imp_X     (EXTERNAL, defined in .idata$5)
	//   7 = _head_<dll>_a (EXTERNAL, undefined; resolves to head member)
	sections := []coffSection{
		{name: ".text", data: text, chars: coffCharText, relocs: []coffReloc{
			{offset: 2, symIdx: 6, typ: coffRelocAMD64REL32}, // → __imp_X
		}},
		{name: ".idata$7", data: make([]byte, 4), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 7, typ: coffRelocAMD64ADDR32NB}, // → _head_<dll>_a
		}},
		{name: ".idata$5", data: make([]byte, 8), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 4, typ: coffRelocAMD64ADDR32NB}, // → .idata$6
		}},
		{name: ".idata$4", data: make([]byte, 8), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 4, typ: coffRelocAMD64ADDR32NB}, // → .idata$6
		}},
		{name: ".idata$6", data: hb.Bytes(), chars: coffCharHint},
	}
	syms := []coffSymbol{
		{name: ".text", section: 1, class: coffSymClassStatic},
		{name: ".idata$7", section: 2, class: coffSymClassStatic},
		{name: ".idata$5", section: 3, class: coffSymClassStatic},
		{name: ".idata$4", section: 4, class: coffSymClassStatic},
		{name: ".idata$6", section: 5, class: coffSymClassStatic},
		{name: symName, section: 1, class: coffSymClassExternal},
		{name: impSym, section: 3, class: coffSymClassExternal},
		{name: headSym, section: 0, class: coffSymClassExternal},
	}
	writeCoffObject(w, machine, sections, syms)
}

// writeBfdDataStub emits a per-symbol stub for a DATA import. Unlike the
// function variant, it has no `.text` thunk and does NOT publish `X` —
// only `__imp_X` (the IAT slot). The plugin's own data definition for X
// has been converted to SHOSTOBJ by peLoadImportLibrary so it carries no
// body; only the host's value is read, via __imp_X.
func writeBfdDataStub(w io.Writer, machine uint16, dllName, symName string, hint uint16) {
	headSym := bfdHeadSym(dllName)
	impSym := "__imp_" + symName

	var hb bytes.Buffer
	var hintBytes [2]byte
	binary.LittleEndian.PutUint16(hintBytes[:], hint)
	hb.Write(hintBytes[:])
	hb.WriteString(symName)
	hb.WriteByte(0)
	if hb.Len()%2 != 0 {
		hb.WriteByte(0)
	}

	// Section indices:
	//   1 = .idata$7    (4-byte ADDR32NB → _head_<dll>_a)
	//   2 = .idata$5    (8-byte IAT slot, ADDR32NB → .idata$6)
	//   3 = .idata$4    (8-byte ILT slot, ADDR32NB → .idata$6)
	//   4 = .idata$6    (hint + name)
	//
	// Symbol indices:
	//   0 = .idata$7    (STATIC section sym)
	//   1 = .idata$5    (STATIC section sym)
	//   2 = .idata$4    (STATIC section sym)
	//   3 = .idata$6    (STATIC section sym)
	//   4 = __imp_X     (EXTERNAL, defined in .idata$5)
	//   5 = _head_<dll>_a (EXTERNAL, undefined; resolves to head member)
	sections := []coffSection{
		{name: ".idata$7", data: make([]byte, 4), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 5, typ: coffRelocAMD64ADDR32NB}, // → _head_<dll>_a
		}},
		{name: ".idata$5", data: make([]byte, 8), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 3, typ: coffRelocAMD64ADDR32NB}, // → .idata$6
		}},
		{name: ".idata$4", data: make([]byte, 8), chars: coffCharIData, relocs: []coffReloc{
			{offset: 0, symIdx: 3, typ: coffRelocAMD64ADDR32NB}, // → .idata$6
		}},
		{name: ".idata$6", data: hb.Bytes(), chars: coffCharHint},
	}
	syms := []coffSymbol{
		{name: ".idata$7", section: 1, class: coffSymClassStatic},
		{name: ".idata$5", section: 2, class: coffSymClassStatic},
		{name: ".idata$4", section: 3, class: coffSymClassStatic},
		{name: ".idata$6", section: 4, class: coffSymClassStatic},
		{name: impSym, section: 2, class: coffSymClassExternal},
		{name: headSym, section: 0, class: coffSymClassExternal},
	}
	writeCoffObject(w, machine, sections, syms)
}

// writeBfdArchive writes the System V symbol index and the head/stubs/tail
// members in BFD long format. The "/" symbol index includes every public
// symbol X, every __imp_X, plus the head-anchor and iname-anchor names so
// ld can pull the right member for any forward reference.
//
// Member name choice: short ASCII names ("h.o", "sNNNNN.o", "t.o"). All fit
// in ar(5)'s 15-char inline name field, so no extended name table ("//") is
// required. ld's SORT(*) on input section name groups members by archive,
// and within an archive sorts by member name (h < s < t), so the head
// sorts before the stubs and the tail sorts after, putting the IAT/ILT
// terminator at the end of the slot run.
func writeBfdArchive(w io.Writer, symbols []impLibSymbol, dllName string, machine uint16) {
	// Pre-build all member object bytes so member sizes (and therefore
	// archive offsets) are known before the "/" symbol index is written.
	var headBuf, tailBuf bytes.Buffer
	writeBfdHead(&headBuf, machine, dllName)
	writeBfdTail(&tailBuf, machine, dllName)

	stubBufs := make([]bytes.Buffer, len(symbols))
	for i, s := range symbols {
		writeBfdStub(&stubBufs[i], machine, dllName, s.name, uint16(i), s.isFunc)
	}

	// Build the symbol-index entries. DATA stubs only publish `__imp_X`
	// (no `.text` thunk, no `X` definition); function stubs publish both.
	type indexEntry struct {
		name   string
		member int // 0 = head, 1 = tail, 2+i = stubs[i]
	}
	entries := make([]indexEntry, 0, 2+2*len(symbols))
	entries = append(entries, indexEntry{bfdHeadSym(dllName), 0})
	entries = append(entries, indexEntry{bfdInameSym(dllName), 1})
	for i, s := range symbols {
		if s.isFunc {
			entries = append(entries, indexEntry{s.name, 2 + i})
		}
		entries = append(entries, indexEntry{"__imp_" + s.name, 2 + i})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	// Compute member offsets in the archive.
	numEntries := len(entries)
	var nameBytesSize int
	for _, e := range entries {
		nameBytesSize += len(e.name) + 1
	}
	linkerSize := 4 + numEntries*4 + nameBytesSize
	linkerPad := linkerSize % 2

	memberOffsets := make([]int, 2+len(symbols))
	pos := 8 + 60 + linkerSize + linkerPad // 8 = "!<arch>\n", 60 = ar header
	for i := range memberOffsets {
		memberOffsets[i] = pos
		var sz int
		switch {
		case i == 0:
			sz = headBuf.Len()
		case i == 1:
			sz = tailBuf.Len()
		default:
			sz = stubBufs[i-2].Len()
		}
		pos += 60 + sz
		if sz%2 != 0 {
			pos++
		}
	}

	// Write "/" symbol index.
	writeArHeader(w, "/", linkerSize)
	binary.Write(w, binary.BigEndian, uint32(numEntries))
	for _, e := range entries {
		binary.Write(w, binary.BigEndian, uint32(memberOffsets[e.member]))
	}
	for _, e := range entries {
		io.WriteString(w, e.name)
		w.Write([]byte{0})
	}
	if linkerPad != 0 {
		w.Write([]byte{'\n'})
	}

	// Write members.
	writeBfdMember(w, "h.o", headBuf.Bytes())
	writeBfdMember(w, "t.o", tailBuf.Bytes())
	for i := range symbols {
		writeBfdMember(w, fmt.Sprintf("s%05d.o", i), stubBufs[i].Bytes())
	}
}

func writeBfdMember(w io.Writer, name string, data []byte) {
	writeArHeader(w, name, len(data))
	w.Write(data)
	if len(data)%2 != 0 {
		w.Write([]byte{'\n'})
	}
}

// peLoadImportLibrary reads a MinGW-compatible import library (.a) and
// registers all symbols in it as dynamic imports from the specified DLL/exe.
// This allows the internal linker to produce a plugin DLL that imports
// runtime symbols from the host executable, without needing an external linker.
//
// The archive's first member is the System V/GNU symbol index ("/"):
//
//	numSyms                  (4 bytes, big-endian)
//	numSyms x 4-byte offsets (big-endian member offsets - unused here)
//	numSyms null-terminated symbol names
//
// Both Go's peGenerateImportLibrary and mingw `ld --out-implib` produce a
// "/" first member in this format. Per-symbol member contents differ by
// toolchain (Go writes IMPORT_OBJECT_HEADER short imports; mingw 2.46+
// writes a compact format) but the symbol index alone names every symbol
// the host exports, so we don't need to parse the per-member contents.
//
// mingw also injects helper names into the symbol index - `.idata$4`,
// `.idata$5`, `.idata$6`, and `__imp_<sym>`. They all match peSkipSymName
// ("." or "_" prefix) and fall out below, leaving just bare exports. The
// DLL name is implicit: import libraries are named "<dll>.a", so the
// basename minus ".a" is the DLL name.
func peLoadImportLibrary(ctxt *Link, implib string) {
	// Resolve relative paths since the linker may run from a different directory.
	if !filepath.IsAbs(implib) {
		if abs, err := filepath.Abs(implib); err == nil {
			implib = abs
		}
	}

	f, err := os.Open(implib)
	if err != nil {
		Exitf("cannot open import library %s: %v", implib, err)
	}
	defer f.Close()

	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "!<arch>\n" {
		Exitf("%s: not a valid ar archive", implib)
	}

	var arHdr [60]byte
	if _, err := io.ReadFull(f, arHdr[:]); err != nil {
		Exitf("%s: missing first archive member", implib)
	}
	memberName := strings.TrimSpace(string(arHdr[0:16]))
	if memberName != "/" {
		Exitf("%s: first archive member is %q, expected \"/\" symbol index", implib, memberName)
	}
	sizeStr := strings.TrimSpace(string(arHdr[48:58]))
	var size int
	if _, err := fmt.Sscanf(sizeStr, "%d", &size); err != nil || size < 4 {
		Exitf("%s: bad symbol-index size %q", implib, sizeStr)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		Exitf("%s: short read of symbol index", implib)
	}

	numSyms := int(binary.BigEndian.Uint32(data[0:4]))
	nameOffset := 4 + numSyms*4
	if nameOffset > len(data) {
		Exitf("%s: corrupt symbol index (numSyms=%d, size=%d)", implib, numSyms, size)
	}
	names := data[nameOffset:]

	dllName := strings.TrimSuffix(filepath.Base(implib), ".a")
	if dllName == "" {
		Exitf("%s: cannot derive DLL name from import library path", implib)
	}

	ldr := ctxt.loader
	importCount := 0
	seen := make(map[string]bool)

	// Collect all names from the symbol index, then process them in
	// two passes (ABI-suffixed first, unsuffixed second). Suffixed-
	// first ordering ensures that when a function appears in host.exe.a
	// as both "X" (ABIInternal entry) and "X.abi0" (ABI0 body), the
	// plugin's v=0 sym is converted with the suffixed extname. The
	// unsuffixed pass then no-ops on the already-SHOSTOBJ sym, so the
	// COFF undef extern carries `.abi0` and mingw ld binds to host's
	// ABI0 body instead of the wrapper, whose `mov %rax,(%rsp)`
	// clobbers the caller's stack arg before tail-calling .abi0.
	var allNames []string
	cursor := 0
	for i := 0; i < numSyms && cursor < len(names); i++ {
		nul := bytes.IndexByte(names[cursor:], 0)
		if nul < 0 {
			break
		}
		allNames = append(allNames, string(names[cursor:cursor+nul]))
		cursor += nul + 1
	}
	sort.Slice(allNames, func(i, j int) bool {
		iSuf := strings.HasSuffix(allNames[i], ".abi0") || strings.HasSuffix(allNames[i], ".abiinternal")
		jSuf := strings.HasSuffix(allNames[j], ".abi0") || strings.HasSuffix(allNames[j], ".abiinternal")
		if iSuf != jSuf {
			return iSuf
		}
		return allNames[i] < allNames[j]
	})

	for _, symName := range allNames {
		// `__imp_X` entries from the host's BFD long-format archive
		// publish the IAT slot for X. With the data-stub variant we
		// emit only `__imp_X` (no `.text` thunk redefining X), so for
		// data imports the symbol index does NOT contain X by itself.
		// Recognize the `__imp_` prefix and route the conversion to X
		// — peSkipSymName would otherwise filter the `__` name out.
		if strings.HasPrefix(symName, "__imp_") {
			symName = strings.TrimPrefix(symName, "__imp_")
		}
		if seen[symName] {
			continue
		}
		seen[symName] = true

		if peSkipSymName(symName) {
			continue
		}
		// Skip function value descriptors (·f symbols). These are DATA
		// containing function pointers. They must stay local so type
		// descriptors can read from them. Their R_ADDR to the actual
		// function will resolve to the PLT stub address.
		if strings.HasSuffix(symName, "\u00b7f") {
			continue
		}
		// Skip linker-defined section boundary markers. These must
		// remain local so the plugin's moduledata points to the DLL's
		// own sections, not the host's.
		if peSectionMarkers[symName] {
			continue
		}

		// Internal link converts host imports to SDYNIMPORT so the
		// internal linker generates IAT entries. External link converts
		// to SHOSTOBJ so the partial PE emits an undefined external
		// reference; gcc/ld then resolves it through host.exe.a (passed
		// positionally on the gcc command line).
		//
		// Both modes convert TEXT (function) symbols so the Go linker's
		// R_CALL/R_PCREL relocations to runtime functions (e.g.,
		// go:link.addmoduledata's call to runtime.addmoduledata) land
		// in the host's runtime, not the plugin's local copy.
		//
		// On amd64, DATA symbols cannot be imported via IAT because the
		// compiler generates direct memory access (LEA/MOV [rip+disp32])
		// with no indirection. Unlike arm64 (which has ADRP+ADD→LDR
		// rewriting for SWINDOWS data), amd64 has no equivalent.
		// Internal link converts ..inittask and runtime.writeBarrier as
		// exceptions because the internal linker handles their access
		// patterns specially. External link skips DATA entirely and
		// keeps a local copy in the plugin — handling indirect data
		// loads cleanly through the IAT requires compiler changes
		// outside this session's scope.
		//
		// host.exe.a's symbol-index names are produced by mangleABIName,
		// so a function with both ABI0 and ABIInternal entries appears
		// twice: once as "X.abi0" (the ABI0 body) and once as "X" (the
		// ABIInternal entry, which on a non-shared host is unsuffixed).
		// Each name maps to exactly one plugin sym version. Route the
		// lookup based on the suffix and preserve the suffix as the
		// converted sym's extname so the COFF undef extern (external
		// link) or IAT-bound name (internal link) matches the host
		// entry mingw ld will resolve us against.
		//
		// Without the suffix routing, ABI0 call sites in the plugin
		// (e.g. the `mov %rdx,(%rsp); call runtime.systemstack` shape
		// in `runtime.(*mheap).alloc`) end up bound to the host's
		// ABIInternal wrapper, whose `mov %rax,(%rsp)` clobbers the
		// caller's stack arg before tail-calling the ABI0 body —
		// causing a PC=0 crash inside systemstack's closure dispatch.
		var ver int = -1
		lookupName := symName
		switch {
		case strings.HasSuffix(symName, ".abi0"):
			ver = 0
			lookupName = strings.TrimSuffix(symName, ".abi0")
		case strings.HasSuffix(symName, ".abiinternal"):
			ver = 1
			lookupName = strings.TrimSuffix(symName, ".abiinternal")
		}

		// External link additionally creates a companion `__imp_X`
		// SHOSTOBJ symbol per converted X. gentext (and the rest of the
		// plugin's code) emits IAT-indirect (`ff 15 disp32`) call sites
		// pointing at __imp_X, which ld resolves to a real Import
		// Directory entry for host.exe. Without this, ld auto-imports the
		// undefined extern via mingw's pseudo-reloc mechanism (32-bit
		// signed delta from patch site to host symbol) which overflows
		// on 64-bit Windows where host and plugin live > 2 GB apart.
		targetType := sym.SDYNIMPORT
		if ctxt.IsExternal() {
			targetType = sym.SHOSTOBJ
		}
		// triedVers controls which plugin sym version(s) to attempt
		// for this host export name. With a suffix the choice is
		// unambiguous; without one:
		//   - external link converts only v=0. Type-descriptor
		//     R_ADDRs target the ABIInternal method body (v=1) on
		//     a non-shared host (mangleABIName leaves it unsuffixed);
		//     converting v=1 here would orphan those relocs ("missing
		//     section for relocation target ...") because the partial
		//     PE has no body for a SHOSTOBJ. The plugin keeps its
		//     local v=1 wrapper, which calls into host's `.abi0` body
		//     through the converted v=0 sym.
		//   - internal link converts both versions; the in-binary IAT
		//     collapses them by name.
		// triedVers controls which plugin sym version(s) to attempt
		// for this host export name.
		//
		// External link normally converts only v=0. Type-descriptor
		// R_METHODOFF/R_ADDROFF relocs target ABIInternal method bodies
		// (v=1) on a non-shared host (mangleABIName leaves them
		// unsuffixed); converting v=1 for those would orphan the relocs
		// at "missing section for relocation target ..." in
		// data.go:relocsym (the SWINDOWS branch at line 500 has special
		// handling but SHOSTOBJ does not).
		//
		// peSharedV1Funcs is a curated allowlist of NON-METHOD package-
		// level functions whose v=1 ABIInternal entry must be host-bound
		// so that FuncPCABIInternal returns a host PC (resolvable by
		// host's findfunc walking host's moduledata) and so plugin's own
		// runtime body for these functions routes into host's. Required
		// for runtime.init.5: it calls FuncPCABIInternal(asyncPreempt2)
		// and (xRegRestore), and findfunc itself must be host-bound so
		// the plugin doesn't walk its own (mostly empty) moduledata.
		var triedVers []int
		if ver >= 0 {
			triedVers = []int{ver}
		} else if ctxt.IsExternal() {
			triedVers = []int{0}
			if peSharedV1Funcs[lookupName] {
				triedVers = []int{0, 1}
			}
		} else {
			triedVers = []int{0, 1}
		}
		for _, v := range triedVers {
			s := ldr.Lookup(lookupName, v)
			if s == 0 {
				continue
			}
			st := ldr.SymType(s)
			if st == sym.SDYNIMPORT || st == sym.SHOSTOBJ {
				continue // Already an import (e.g., Windows API)
			}
			// DATA gating. With -dynlink the compiler emits R_GOTPCREL
			// for every extern data access and peRetargetGotRelocs
			// retargets those to __imp_X via peGotRedirect. For each
			// host-shared DATA we convert here, the plugin's references
			// resolve to host's value through the OS-loader-populated
			// IAT slot.
			//
			// Always-share patterns:
			//   runtime.tls_g           — Windows TLS slot index (Bug 8)
			//   runtime.writeBarrier    — GC write-barrier state
			//   runtime.lastmoduledatap — module list tail
			//   *..inittask             — package init lists
			//
			// Plus peSharedRuntimeData for the rest of the runtime
			// singletons (mheap_, sched, gcController, etc.). Internal
			// link adds the original four as the existing exceptions for
			// initdynimport's IAT-data-slot transform; external link
			// shares the same superset.
			//
			// Package-data sharing for testplugin/common.X (so host's
			// init-side mutations are visible to plugin readers) is NOT
			// addressed by this curation alone — broadening DATA conversion
			// or sharing user-package ..inittasks introduces GC-state /
			// forcegc-phase / sweepgen regressions. See v3 roadmap.
			if !st.IsText() {
				share := strings.HasSuffix(lookupName, "..inittask") ||
					lookupName == "runtime.writeBarrier" ||
					lookupName == "runtime.tls_g" ||
					lookupName == "runtime.lastmoduledatap" ||
					peSharedRuntimeData[lookupName]
				if !share {
					continue
				}
			}
			sb := ldr.MakeSymbolUpdater(s)
			sb.SetType(targetType)
			sb.SetSize(0)
			sb.SetData(nil)
			sb.ResetRelocs()
			ldr.FreeData(s)
			if targetType == sym.SDYNIMPORT {
				ldr.SetSymDynimplib(s, dllName)
			}
			// Preserve the host export name (including any ABI suffix)
			// as the converted sym's extname. writeSymbols and gentext
			// use SymExtname for SHOSTOBJ/SDYNIMPORT so the plugin
			// references the right host entry.
			ldr.SetSymExtname(s, symName)
			// Mark reachable so initdynimport creates IAT entries
			// (internal) or the COFF writer emits an undefined extern
			// (external).
			ldr.SetAttrReachable(s, true)
			importCount++

			if ctxt.IsExternal() {
				// Don't mark __imp_X reachable here — the deadcode pass
				// runs after peLoadImportLibrary and would clear the bit.
				// Whoever adds the reloc to __imp_X (gentext) marks it
				// reachable at that point.
				impSym := ldr.LookupOrCreateSym("__imp_"+symName, 0)
				impSb := ldr.MakeSymbolUpdater(impSym)
				impSb.SetType(sym.SHOSTOBJ)
				// External link binds each host name to exactly one
				// plugin sym; further fallback would set extname twice.
				break
			}
		}
	}
	if ctxt.Debugvlog > 0 {
		ctxt.Logf("plugin: loaded %d import symbols from %s\n", importCount, implib)
	}
}

// filterConvertedSyms removes SDYNIMPORT, SHOSTOBJ, and SWINDOWS symbols
// from Textp. These are host-provided symbols that were converted from
// archive defs and no longer have a local body to emit. SHOSTOBJ covers
// the external-link plugin case where peLoadImportLibrary converts
// runtime.addmoduledata so the partial PE emits it as an undefined extern.
func filterConvertedSyms(ctxt *Link) []loader.Sym {
	ldr := ctxt.loader
	j := 0
	for _, s := range ctxt.Textp {
		st := ldr.SymType(s)
		if st != sym.SDYNIMPORT && st != sym.SHOSTOBJ && st != sym.SWINDOWS {
			ctxt.Textp[j] = s
			j++
		}
	}
	return ctxt.Textp[:j]
}

// peExportAllSymbols marks all reachable text/data symbols for export in
// the PE export table. This is the internal-linker equivalent of MinGW's
// --export-all-symbols flag.
func peExportAllSymbols(ctxt *Link) {
	ldr := ctxt.loader
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		st := ldr.SymType(s)
		if !st.IsText() && !st.IsRODATA() && !st.IsData() {
			continue
		}
		name := ldr.SymExtname(s)
		if peSkipSymName(name) {
			continue
		}
		// Skip symbols that are already in the export list.
		if ldr.AttrCgoExportDynamic(s) {
			continue
		}
		ldr.SetAttrCgoExportDynamic(s, true)
	}
}

