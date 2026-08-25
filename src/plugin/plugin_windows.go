// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"errors"
	"internal/pluginabi"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// kernel32!GetModuleFileNameW resolves a loaded module handle back to
// the path the loader recorded for it; open uses it to detect the
// base-name aliasing described on trendvidia/go#40. A LazyDLL keeps
// package plugin off internal/syscall/windows, whose import would put
// it in cmd/dist's bootstrap copy set (buildtool.go).
var (
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleFileNameW = modkernel32.NewProc("GetModuleFileNameW")
)

func open(name string) (*Plugin, error) {
	absPath, err := filepath.Abs(name)
	if err != nil {
		return nil, errors.New(`plugin.Open("` + name + `"): ` + err.Error())
	}
	pluginPath := filepath.Clean(absPath)

	pluginsMu.Lock()
	if p := plugins[pluginPath]; p != nil {
		// p.err transitions "" → non-empty only under pluginsMu
		// (failed-load caching, Plugin.Close), so it must be read
		// while still holding the mutex. Release before waiting on
		// loaded so other plugins can load/close concurrently; a
		// Close completing in that window hands back an already-
		// closed *Plugin, on which every later operation safely
		// returns errors.
		err := p.err
		pluginsMu.Unlock()
		if err != "" {
			return nil, errors.New(`plugin.Open("` + name + `"): ` + err + ` (previous failure)`)
		}
		<-p.loaded
		return p, nil
	}

	// Cross-toolchain ABI fence, stage 1 (trendvidia/go#24, part 2):
	// reject a binary from an incompatible toolchain generation by
	// reading its go:plugin.abi marker straight from the file — before
	// LoadLibrary maps anything, so the candidate's DllMain (which
	// appends its moduledata and can run arbitrary constructors) never
	// executes for a rejected DLL. Unreadable files fall through so
	// LoadLibrary keeps producing the canonical OS error.
	//
	// Judging file bytes alone cannot tell WHICH module the loader will
	// return: the file may be swapped between this read and the load,
	// and a request can resolve to an already-mapped module elsewhere
	// on disk (trendvidia/go#40). The two post-load checks below — the
	// handle's recorded path against the request, and the marker
	// re-read from the mapped image — are the authority on what was
	// actually loaded; this stage remains the gate that keeps any
	// rejected candidate's code from ever running.
	//
	// This sits AFTER the cache lookup above and before LoadLibrary, not
	// ahead of both: Open documents that "if a path has already been
	// opened, then the existing *Plugin is returned" (plugin.go), and an
	// already-mapped module is not a candidate for loading. Fencing
	// ahead of the cache would re-read the whole DLL on every repeat
	// Open, and would fail a cached, still-valid *Plugin whose file had
	// since been overwritten on disk.
	if err := checkABIMarker(pluginPath); err != nil {
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + err.Error())
	}

	// Snapshot BEFORE loading: addmoduledata chains the DLL's
	// moduledata from its entry point inside LoadLibrary (measured on
	// windows/arm64: the tail always moves across this call for a
	// healthy plugin), so an unchanged tail afterwards means the DLL
	// never initialized here.
	prevTail := chainTail()
	h, loadErr := syscall.LoadLibrary(pluginPath)
	if loadErr != nil {
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + loadErr.Error())
	}

	if len(name) > 4 && name[len(name)-4:] == ".dll" {
		name = name[:len(name)-4]
	}
	if plugins == nil {
		plugins = make(map[string]*Plugin)
	}

	// The chain did not grow. Two distinct causes, distinguished by
	// where the returned handle actually lives:
	//
	//   - The returned handle belongs to a DIFFERENT module than
	//     requested — an already-mapped DLL handed back instead of a
	//     fresh map (the behaviour Microsoft documents for same-named
	//     modules), leaving the process one DllMain short of what the
	//     caller asked for (trendvidia/go#40). Rejecting with both
	//     paths named beats the silent wrong-path→module association a
	//     success would cache — and beats misreporting it as the
	//     no-module-data case below. The FreeLibrary is still taken: an
	//     aliased module is held by its original opener, so dropping
	//     only this call's own reference can never unmap it. (Measured
	//     on Windows 11 arm64, 2026-08: two plugins sharing one base
	//     name in different directories both load as their own modules,
	//     so this branch guards a rule other loaders/SKUs document
	//     rather than one this build exhibits.)
	//
	//   - A freshly mapped DLL registered no Go module data in this
	//     process (its runtime imports may be bound to another
	//     program's image); nothing chained here, so unmapping it is
	//     safe. If GetModuleFileNameW could not tell us where the
	//     handle lives, this is the behaviour we fall back to — for an
	//     undetected alias that drop is equally harmless.
	//
	// validatePkghashes must not run in either case: on a chain that
	// did not grow it would take the last healthy peer for the newly
	// chained module — silently passing a divergent build, or
	// marking/detaching that peer's moduledata while the FreeLibrary
	// below unmaps only the offender. No syncBeforeUnmap is needed:
	// nothing entered activeModules, so no GC root references these
	// images.
	//
	// The path comparison belongs HERE and not above the chain-growth
	// test. A load that DID grow the chain mapped a fresh image, so
	// stage 2 below judges that image's own marker and no aliasing is
	// possible; all a path comparison could add there is a rejection of
	// spellings the loader resolved legitimately — an 8.3 short name, a
	// junction, a symlink — which today load fine. Rejection is the
	// right answer only where it replaces a silent alias, which is this
	// branch.
	if chainTail() == prevTail {
		if loaded := loadedModulePath(h); loaded != "" && !sameModulePath(loaded, pluginPath) {
			aliased := pluginPath + " resolved to a different module, already loaded from " + loaded
			plugins[pluginPath] = &Plugin{err: aliased}
			syscall.FreeLibrary(h)
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + aliased)
		}
		const noModData = "plugin DLL registered no Go module data in this process" +
			" (its runtime imports may be bound to another program's image)"
		syscall.FreeLibrary(h)
		plugins[pluginPath] = &Plugin{err: noModData}
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + noModData)
	}

	// Cross-toolchain ABI fence, stage 2: re-read the marker from the
	// module the loader ACTUALLY mapped, through its live export table.
	// This closes what judging file bytes cannot: a file swapped in
	// between the stage-1 read and LoadLibrary has its replacement
	// mapped and judged here by its real contents (trendvidia/go#40).
	// Reaching this point means the chain grew above, i.e. the module's
	// DllMain ran and appended its moduledata. Unmapping such an image
	// safely needs the stop-the-world barrier Close takes
	// (syncBeforeUnmap), so the rejection keeps the mapping resident
	// rather than freeing — repeat Opens hit the cached failure below
	// and never map again — but a chained node must not stay WALKABLE:
	// pluginabi.Version bumps precisely when moduledata layout
	// assumptions change, so every later chain walk reading that node
	// at host offsets is exactly the fatal-fault hazard the fence
	// exists to prevent. rejectChainedModule therefore truncates the
	// chain back to prevTail and rebuilds activeModules, identifying
	// the residue by position rather than by reading any field of it.
	// That bounds every walk starting after the truncation; the window
	// opened at DllMain, before Open could check anything, and cannot
	// be closed from here. This check runs before validatePkghashes and
	// the cgo-notify poke so neither touches a module we are about to
	// refuse.
	if err := checkLoadedABIMarker(h); err != nil {
		rejectChainedModule(prevTail)
		plugins[pluginPath] = &Plugin{err: err.Error()}
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + err.Error())
	}

	if proc, err := syscall.GetProcAddress(h, "go:plugin.pkghashes"); err == nil {
		if errstr, rejectedPath := validatePkghashes(uintptr(proc)); errstr != "" {
			// rejectedPath's bytes live inside the rejected DLL's image;
			// clone them before FreeLibrary unmaps those pages — the
			// same dangling-header hazard closePlugin clones
			// p.pluginpath against (see cloneString in plugin.go).
			rejectedPath = cloneString(rejectedPath)
			// validatePkghashes marked the rejected moduledata bad AND
			// detached it from the chain — it lives inside this DLL's
			// image, so it must be gone from the chain before the
			// FreeLibrary below unmaps those pages. syncBeforeUnmap is
			// the same STW barrier Close takes: no GC worker may still
			// be scanning this module's data roots when they free.
			syncBeforeUnmap()
			syscall.FreeLibrary(h)
			// Cache the failure like the lastmoduleinit-failure path
			// does: a bare return would leak one loader refcount per
			// retry and re-run DllMain each time, deterministically
			// failing again.
			plugins[pluginPath] = &Plugin{
				pluginpath: rejectedPath,
				err:        errstr,
			}
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + errstr)
		}
	}

	// Wake up the plugin's own cgo runtime-init handshake. Plugin DLLs
	// statically link their own copy of runtime/cgo, including the
	// runtime_init_done flag and runtime_init_wait Event in
	// gcc_libinit_windows.c. Host's runtime.main called notify on host's
	// copy at startup; plugin has no runtime.main of its own, so plugin's
	// flag stays 0 and any cgo entry-point in the plugin would block
	// forever in _cgo_wait_runtime_init_done.
	//
	// Invoke plugin's notify ourselves. The function is mingw-auto-
	// exported from any plugin DLL that contains runtime/cgo (i.e. has
	// `import "C"` somewhere); pure-Go plugins don't export it and we
	// silently skip via GetProcAddress's not-found error. The notify
	// function is idempotent (gated by InterlockedExchangeAdd) and
	// touches only this DLL's local statics — host is unaffected.
	if proc, err := syscall.GetProcAddress(h, "x_cgo_notify_runtime_init_done"); err == nil {
		syscall.SyscallN(uintptr(proc), 0)
	}

	pluginpath, syms, initTasks, errstr := lastmoduleinit()
	if errstr != "" {
		plugins[pluginPath] = &Plugin{
			pluginpath: pluginpath,
			err:        errstr,
		}
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + errstr)
	}
	// This function can be called from the init function of a plugin.
	// Drop a placeholder in the map so subsequent opens can wait on it.
	p := &Plugin{
		pluginpath: pluginpath,
		loaded:     make(chan struct{}),
	}
	plugins[pluginPath] = p
	pluginsMu.Unlock()

	doInit(initTasks)

	// Fill out the value of each plugin symbol.
	updatedSyms := map[string]any{}
	var funcDescriptors []*uintptr
	for symName, sym := range syms {
		isFunc := symName[0] == '.'
		if isFunc {
			delete(syms, symName)
			symName = symName[1:]
		}

		fullName := pluginpath + "." + symName
		proc, procErr := syscall.GetProcAddress(h, fullName)
		if procErr != nil {
			// p is already cached with loaded unclosed; a bare return
			// strands every future Open of this path on <-p.loaded
			// forever (and handed the caller no *Plugin to Close).
			// Mark the entry failed and wake waiters; subsequent
			// Opens report "(previous failure)". The library stays
			// mapped and the handle unreleased — no *Plugin with a
			// live handle escaped, so nothing may FreeLibrary it.
			errStr := "could not find symbol " + symName + ": " + procErr.Error()
			pluginsMu.Lock()
			p.err = errStr
			close(p.loaded)
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + errStr)
		}
		valp := (*[2]unsafe.Pointer)(unsafe.Pointer(&sym))
		if isFunc {
			// Allocate a stable heap holder (mirroring plugin_dlopen's
			// pattern). The func value's data pointer is the holder;
			// calling the value loads holder[0] and indirects to it.
			// At Close, the runtime overwrites *holder with the
			// pluginClosedFuncStub PC, so any held call through this
			// Lookup func value panics with a recognisable message
			// instead of jumping into what FreeLibrary has just
			// unmapped. Runtime-created closures returned by calls
			// into the plugin carry no such holder and stay
			// unprotected.
			holder := new(uintptr)
			*holder = proc
			(*valp)[1] = unsafe.Pointer(holder)
			funcDescriptors = append(funcDescriptors, holder)
		} else {
			(*valp)[1] = unsafe.Pointer(proc)
		}
		// we can't add to syms during iteration as we'll end up processing
		// some symbols twice with the inability to tell if the symbol is a function
		updatedSyms[symName] = sym
	}
	p.syms = updatedSyms
	p.funcDescriptors = funcDescriptors
	p.handle = uintptr(h)

	close(p.loaded)
	return p, nil
}

// lookup serialises against Plugin.Close via pluginsMu: closePlugin
// mutates p.syms (nils it), p.pluginpath (clones it off the plugin's
// RODATA before FreeLibrary unmaps those bytes) and p.err while
// holding the mutex, so an unlocked read here could race a concurrent
// Close — torn map access at best, a pluginpath dangling into
// unmapped memory in the error path at worst. Lookup never runs with
// pluginsMu already held (open and closePlugin do not call it), so
// taking the mutex here cannot deadlock.
func lookup(p *Plugin, symName string) (Symbol, error) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	if s := p.syms[symName]; s != nil {
		return s, nil
	}
	return nil, errors.New("plugin: symbol " + symName + " not found in plugin " + p.pluginpath)
}

// unloadPlugin is the Windows counterpart of plugin_dlopen.go's
// dlclose-based phase 2. Calls FreeLibrary on the HMODULE captured
// at Open time, then drops the plugin from the global registry. The
// runtime side of Plugin.Close (plugin_close in runtime/plugin.go)
// already rewrote every registered func descriptor to
// pluginClosedFuncStub before this point, so a held call through a
// Lookup-obtained func value panics with a recognisable message
// instead of jumping into what FreeLibrary just unmapped.
// Unregistered references — closures returned by calls into the
// plugin, pointers to exported variables — still point into this
// image and fault unrecoverably if used now; see Plugin.Close.
//
// Caller (closePlugin) holds pluginsMu.
func unloadPlugin(p *Plugin) error {
	if p.handle == 0 {
		return nil
	}
	if err := syscall.FreeLibrary(syscall.Handle(p.handle)); err != nil {
		return err
	}
	for path, q := range plugins {
		if q == p {
			delete(plugins, path)
			break
		}
	}
	return nil
}

// lastmoduleinit is defined in package runtime via go:linkname.
//
//go:linkname lastmoduleinit
func lastmoduleinit() (pluginpath string, syms map[string]any, inittasks []*initTask, errstr string)

// validatePkghashes is defined in runtime/plugin.go via go:linkname.
// addr is the address (from GetProcAddress) of the plugin's
// go:plugin.pkghashes export, i.e. a slice header for the plugin's
// pkghashes carrier. Returns an error message describing the first
// mismatched package ("" on success), plus the rejected module's
// pluginpath for failure diagnostics ("") on success.
//
// On mismatch it also marks the rejected moduledata bad and detaches
// it from the chain, so the caller may FreeLibrary the DLL safely.
//
//go:linkname validatePkghashes
func validatePkghashes(addr uintptr) (errstr string, rejectedPath string)

// chainTail is defined in package runtime via go:linkname. It returns
// the address of the last moduledata on the chain; open() snapshots it
// around LoadLibrary to detect whether the DLL registered here.
//
//go:linkname chainTail
func chainTail() uintptr

// rejectChainedModule is defined in package runtime via go:linkname.
// The stage-2 ABI check rejects a module whose DllMain has already
// chained its moduledata but which must not be freed (see open); this
// truncates the chain back to prevTail — open's pre-LoadLibrary
// chainTail snapshot — so no chain walk, peer scan or activeModules
// rebuild that starts afterwards reads the rejected module's
// foreign-layout memory at host offsets. The node is identified by
// position, never by inspecting it: its fields are exactly what the
// generation mismatch says may have moved.
//
//go:linkname rejectChainedModule
func rejectChainedModule(prevTail uintptr)

// checkABIMarker enforces the cross-toolchain ABI fence: parse
// pluginPath's PE export table from the raw FILE bytes and require a
// go:plugin.abi export whose {pluginabi.Marker, pluginabi.Version}
// matches this toolchain's constants. Everything is resolved through
// static file offsets — nothing is mapped and no code from the DLL
// runs — so rejection precedes the DLL's DllMain entirely.
//
// The marker exists because every later check assumes both sides agree
// on moduledata layout: a DLL built by a different fork/toolchain
// generation appends its moduledata without complaint (its DllMain
// touches only its own image), and the process then dies fatally when
// the host walks that foreign memory at its own field offsets. The
// bump rules for Version live in internal/pluginabi.
//
// Fail-closed for anything that IS readable: malformed PE, missing or
// unrecognised marker, or wrong version all reject. Only a missing path
// or a denied open returns nil so LoadLibrary reports its canonical OS
// error, preserving pre-fence behaviour for that case.
//
// What file bytes cannot decide — WHICH module LoadLibrary will return
// (a file swapped in after this read; a handle resolved to an
// already-mapped module elsewhere on disk) — is settled after the
// load: open() compares the returned handle's recorded path against
// the request and re-reads this same marker from the mapped image via
// GetProcAddress (checkLoadedABIMarker). This stage remains the only
// one that can keep a rejected candidate's DllMain from ever running;
// the post-load checks are the authority on what was mapped instead.
func checkABIMarker(pluginPath string) error {
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		// Fall through only for the failures LoadLibrary reports
		// identically anyway — a missing path or a denied open — so its
		// canonical OS error survives. Anything else (a sharing
		// violation while a build holds the DLL open, transient I/O, an
		// allocation failure on a huge image) must not silently disable
		// the fence and hand an unverified file to LoadLibrary: that is
		// exactly the mapping the fence exists to prevent.
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return errors.New("cannot read plugin binary for the toolchain ABI check: " + err.Error())
	}
	// Little-endian readers with bounds guards; hand-rolled so package
	// plugin stays below encoding/binary in go/build's dependency policy.
	le16 := func(off int) uint32 {
		if off < 0 || off+2 > len(data) {
			return 0
		}
		return uint32(data[off]) | uint32(data[off+1])<<8
	}
	le32 := func(off int) uint32 {
		if off < 0 || off+4 > len(data) {
			return 0
		}
		return uint32(data[off]) | uint32(data[off+1])<<8 |
			uint32(data[off+2])<<16 | uint32(data[off+3])<<24
	}

	if len(data) < 0x40 || string(data[:2]) != "MZ" {
		return errors.New("plugin binary is not a PE image")
	}
	peOff := int(le32(0x3c))
	if peOff <= 0 || peOff+24 > len(data) || string(data[peOff:peOff+4]) != "PE\x00\x00" {
		return errors.New("plugin binary is not a PE image")
	}
	coff := peOff + 4
	nsect := int(le16(coff + 2))
	optOff := coff + 20
	optSize := int(le16(coff + 16))
	if le16(optOff) != 0x20b { // IMAGE_NT_OPTIONAL_HDR64_MAGIC
		return errors.New("plugin binary is not a PE32+ image")
	}

	// Section table: build the RVA→file-offset mapping. Export-table
	// structures are addressed by RVA; their bytes live in sections.
	sectTbl := optOff + optSize
	if sectTbl < 0 || sectTbl+nsect*40 > len(data) {
		return errors.New("plugin binary has a malformed section table")
	}
	type section struct{ va, raw, ptr uint32 }
	sects := make([]section, 0, nsect)
	for i := 0; i < nsect; i++ {
		h := sectTbl + i*40
		va, raw, ptr := le32(h+12), le32(h+16), le32(h+20)
		// Only a section whose bytes are actually present in the file can
		// answer an RVA. A section that cannot — unbacked data with
		// SizeOfRawData==0, unbacked data with a rounded SizeOfRawData
		// and PointerToRawData==0 (the shape this fork's own PE writer
		// emits for .bss, pe.go's asmbPe), or one mapped at RVA 0 — is
		// dropped from the map rather than rejecting the whole image.
		// Dropping still fails closed: a marker reachable only through
		// such a section stays unresolvable and Open refuses. Rejecting
		// outright is how this fence broke every cgo plugin once already
		// (commit 13317ee199), and one unusual section header is not
		// evidence about the section the marker actually lives in.
		if va == 0 || raw == 0 || ptr == 0 || int(ptr) >= len(data) {
			continue
		}
		// A section may claim more raw bytes than the file holds; clamp
		// so an RVA never maps past EOF.
		if uint64(ptr)+uint64(raw) > uint64(len(data)) {
			raw = uint32(uint64(len(data)) - uint64(ptr))
		}
		sects = append(sects, section{va: va, raw: raw, ptr: ptr})
	}
	rvaToOff := func(rva uint32) int {
		for _, s := range sects {
			if uint64(s.va) <= uint64(rva) && uint64(rva) < uint64(s.va)+uint64(s.raw) {
				return int(s.ptr) + int(rva-s.va)
			}
		}
		return -1
	}

	expRVA := le32(optOff + 112) // data directory[0]: export table
	if expRVA == 0 {
		return errors.New("missing go:plugin.abi marker (not built by this fork's toolchain?)")
	}
	expOff := rvaToOff(expRVA)
	if expOff < 0 {
		return errors.New("plugin binary has an unreadable export directory")
	}
	numFuncs := le32(expOff + 20)
	numNames := le32(expOff + 24)
	// The three export arrays are RVAs too — convert their starts to
	// file offsets once, then index within the mapped file. Indexing by
	// the raw RVA would read past EOF (bounds-guarded reads return 0).
	funcsTbl := rvaToOff(le32(expOff + 28))
	namesTbl := rvaToOff(le32(expOff + 32))
	ordsTbl := rvaToOff(le32(expOff + 36))
	if funcsTbl < 0 || namesTbl < 0 || ordsTbl < 0 {
		return errors.New("plugin binary has an unreadable export directory")
	}
	// NumberOfNames and NumberOfFunctions come from the candidate file
	// and are otherwise unbounded. A corrupt or hostile DLL claiming
	// 0xFFFFFFFF names would spin the loop below ~4e9 times, each doing
	// a linear section scan — the fence hanging Open instead of
	// rejecting it, which is failing open on availability against
	// exactly the input class it exists to judge. Every array indexed by
	// those counts must fit inside the file; that is the bound.
	if uint64(namesTbl)+uint64(numNames)*4 > uint64(len(data)) ||
		uint64(ordsTbl)+uint64(numNames)*2 > uint64(len(data)) ||
		uint64(funcsTbl)+uint64(numFuncs)*4 > uint64(len(data)) {
		return errors.New("plugin binary has a malformed export directory")
	}

	cstr := func(off int) string {
		if off < 0 || off >= len(data) {
			return ""
		}
		end := off
		for end < len(data) && data[end] != 0 && end-off < 128 {
			end++
		}
		return string(data[off:end])
	}
	for i := uint32(0); i < numNames; i++ {
		nameOff := rvaToOff(le32(namesTbl + int(i)*4))
		if cstr(nameOff) != "go:plugin.abi" {
			continue
		}
		// The name matched, so this entry's ordinal must index the
		// export address table; one that does not means a broken
		// directory, not a different symbol to keep looking for.
		ordIdx := int(le16(ordsTbl + int(i)*2))
		if uint32(ordIdx) >= numFuncs {
			return errors.New("go:plugin.abi marker is unreadable")
		}
		valOff := rvaToOff(le32(funcsTbl + ordIdx*4))
		if valOff < 0 {
			return errors.New("go:plugin.abi marker is unreadable")
		}
		marker, version := le32(valOff), le32(valOff+4)
		return abiMarkerMismatch(marker, version, false)
	}
	return errors.New("missing go:plugin.abi marker (not built by this fork's toolchain?)")
}

// abiMarkerMismatch reports the fence error for a {pluginabi.Marker,
// pluginabi.Version} pair that failed this toolchain's generation
// check. Both fence stages describe the same defect and differ only in
// where their bytes came from, so the wording lives here once: loaded
// selects the phrasing for a pair read from the mapped image (stage 2)
// over one parsed from the file (stage 1).
func abiMarkerMismatch(marker, version uint32, loaded bool) error {
	subject := "plugin"
	inImage := ""
	if loaded {
		subject = "the loaded plugin module"
		inImage = " in the loaded module"
	}
	switch {
	case marker != pluginabi.Marker:
		return errors.New("unrecognised go:plugin.abi marker" + inImage +
			" (not built by this fork's toolchain)")
	case version != pluginabi.Version:
		return errors.New(subject + " was built with an incompatible toolchain ABI (plugin ABI " +
			strconv.FormatUint(uint64(version), 10) +
			", host ABI " + strconv.FormatUint(uint64(pluginabi.Version), 10) + ")")
	}
	return nil
}

// checkLoadedABIMarker is stage 2 of the cross-toolchain ABI fence:
// read {pluginabi.Marker, pluginabi.Version} back from the module the
// loader actually mapped — h, not the file at any path — and require
// this toolchain's generation. It runs after open()'s chain-growth
// check, so reaching it means the DLL initialized in this process;
// callers must therefore NEVER FreeLibrary on its rejection (see the
// stage-2 comment in open) and must instead rejectChainedModule so the
// chained node stops being walkable.
func checkLoadedABIMarker(h syscall.Handle) error {
	proc, err := syscall.GetProcAddress(h, "go:plugin.abi")
	if err != nil {
		return errors.New("the loaded plugin module exports no go:plugin.abi marker" +
			" (not built by this fork's toolchain?)")
	}
	marker := *(*uint32)(unsafe.Pointer(proc))
	version := *(*uint32)(unsafe.Pointer(proc + 4))
	return abiMarkerMismatch(marker, version, true)
}

// loadedModulePath returns the full path the loader recorded for the
// mapped module h, or "" if that cannot be determined (callers fall
// back to their pre-check behaviour rather than rejecting on missing
// evidence).
func loadedModulePath(h syscall.Handle) string {
	for size := uint32(1024); size <= 65536; size *= 2 {
		buf := make([]uint16, size)
		r0, _, _ := procGetModuleFileNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
		// GetModuleFileNameW returns a DWORD, and the Windows x64 ABI
		// leaves the upper half of RAX undefined for one; narrow before
		// using it as a length, exactly as the generated wrappers in
		// internal/syscall/windows do. An unnarrowed value would index
		// out of range, and open holds pluginsMu across this call with
		// no deferred unlock, so that panic would wedge every later
		// Open, Lookup and Close in the process.
		n := uint32(r0)
		if n == 0 {
			return ""
		}
		if n >= size {
			// Truncated (os.getModuleFileName's idiom): the result
			// filled the buffer, so retry with a larger one rather than
			// trusting the last-error report.
			continue
		}
		return syscall.UTF16ToString(buf[:n])
	}
	return ""
}

// sameModulePath compares a loader-recorded module path with the path
// Open was given, forgiving spelling differences that do not name a
// different file: letter case (Windows paths are case-insensitive),
// forward slashes, and the \\?\ literal-device prefix. A mismatch is
// therefore a genuinely different module. Exotic-but-equivalent
// spellings this comparison does not model (8.3 short names,
// junctions, \\?\UNC\) degrade to a rejection whose message names both
// spellings — loud and diagnosable, never a silent alias.
func sameModulePath(a, b string) bool {
	norm := func(p string) string {
		p = strings.ReplaceAll(p, "/", "\\")
		p = strings.TrimPrefix(p, `\\?\`)
		return p
	}
	return strings.EqualFold(norm(a), norm(b))
}

// doInit is defined in package runtime.
//
//go:linkname doInit runtime.doInit
func doInit(t []*initTask)

type initTask struct {
	// fields defined in runtime.initTask. We only handle pointers to an initTask
	// in this package, so the contents are irrelevant.
}
