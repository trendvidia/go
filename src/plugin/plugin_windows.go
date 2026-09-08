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
	procGetLongPathNameW   = modkernel32.NewProc("GetLongPathNameW")
	procVirtualAlloc       = modkernel32.NewProc("VirtualAlloc")
)

func open(name string) (*Plugin, error) {
	absPath, err := filepath.Abs(name)
	if err != nil {
		return nil, errors.New(`plugin.Open("` + name + `"): ` + err.Error())
	}
	pluginPath := filepath.Clean(absPath)
	// The cache is keyed by the file's identity, not by its spelling.
	// The loader resolves an 8.3 short name (RUNNER~1) to the same
	// module as its long form, so a second Open through the other
	// spelling must find what the first one cached. Missing it
	// LoadLibrary's an already-mapped module, which returns the
	// existing handle without re-running DllMain — landing in the
	// chain-did-not-grow branch below, whose two causes cannot tell
	// that case from a fresh map that registered no module data — and
	// caches a false diagnosis under the second spelling forever,
	// instead of returning the existing *Plugin that Open documents
	// ("if a path has already been opened, then the existing *Plugin is
	// returned", plugin.go). Only the KEY is canonicalised: the fence
	// still reads, and LoadLibrary still maps, the spelling the caller
	// gave, so a short name that keeps a deep path inside MAX_PATH
	// keeps loading. Spellings longPathName cannot expand (a file that
	// does not exist yet) key by themselves, as before.
	//
	// LIMITATION, deliberate (trendvidia/go#100): the key is NOT
	// case-folded. GetLongPathNameW canonicalises the case of every
	// component it resolves, but not the drive letter, so
	// Open(`c:\dir\p.dll`) after Open(`C:\dir\p.dll`) still misses the
	// cache — the second Open re-LoadLibrary's the already-mapped
	// module and caches the chain-did-not-grow diagnosis under its own
	// spelling. Folding case here would close that, and is not done
	// because Windows paths are only case-insensitive by convention: a
	// directory carrying the NTFS per-directory case-sensitivity flag
	// (fsutil file setCaseSensitiveInfo) really does hold p.dll and
	// P.DLL as two files, and folding would then alias two distinct
	// plugins onto one cache entry. A false cache MISS is a loud,
	// diagnosable error; a false cache HIT hands a caller the wrong
	// module. The trade is decided in favour of the miss.
	//
	// Callers that may spell a path more than one way should
	// canonicalise it once before Open; the doc comment on Open says
	// so.
	pluginKey := longPathName(pluginPath)

	pluginsMu.Lock()
	if p := plugins[pluginKey]; p != nil {
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
		// The load we waited on may have failed after the read
		// above: the could-not-find-symbol path below marks the
		// cached entry failed and then closes loaded, so a waiter
		// that arrived while that load was in flight would
		// otherwise be handed (p, nil) for a *Plugin whose syms are
		// nil and whose Close silently no-ops — a failed load
		// reported as a success. Re-read p.err under the mutex and
		// report it. closedErr is excluded on purpose: that is the
		// Close-in-the-window case described above, which keeps
		// handing back the already-closed *Plugin.
		pluginsMu.Lock()
		err = p.err
		pluginsMu.Unlock()
		if err != "" && err != closedErr {
			return nil, errors.New(`plugin.Open("` + name + `"): ` + err + ` (previous failure)`)
		}
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
	if hook := testHookBetweenFenceStages; hook != nil {
		callFenceHook(hook, pluginPath)
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
	//
	// Growth is judged from prevTail's own next link and never by walking
	// to the new tail: prevTail is host-shaped (the host module or a
	// plugin that already passed the fence), but the node a foreign
	// generation just appended is exactly what stage 2 below has not yet
	// judged, and reading ITS next word at host offsets is the fault the
	// fence exists to prevent.
	if !chainGrew(prevTail) {
		if loaded := loadedModulePath(h); loaded != "" && !sameModulePath(loaded, pluginPath) {
			aliased := pluginPath + " resolved to a different module, already loaded from " + loaded
			// Unlike every other failure open caches, this one is NOT
			// written into plugins: a cached Open failure is a property
			// of the FILE (bad ABI marker, no module data, pkghash
			// divergence — stable across retries), but the alias
			// rejection is process state that changes the moment the
			// blocking module is closed. Caching it would leave the
			// path unloadable for the life of the process, naming a
			// module that is no longer loaded (trendvidia/go#46). The
			// cost of re-evaluating is one LoadLibrary/FreeLibrary
			// refcount bump on an already-mapped image per retry.
			syscall.FreeLibrary(h)
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + aliased)
		}
		const noModData = "plugin DLL registered no Go module data in this process" +
			" (its runtime imports may be bound to another program's image)"
		syscall.FreeLibrary(h)
		plugins[pluginKey] = &Plugin{err: noModData}
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
		plugins[pluginKey] = &Plugin{err: err.Error()}
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
			plugins[pluginKey] = &Plugin{
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
		plugins[pluginKey] = &Plugin{
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
	plugins[pluginKey] = p
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

// lookup resolves p to the live cache entry (liveOrigin) and reads
// syms/pluginpath off THAT entry, never off p: p may be a struct
// copy whose fields outlived the original's Close, where the syms
// map's keys and the pluginpath header both point into the unmapped
// image (trendvidia/go#64). On the resolving path the only field
// lookup reads off p is p.loaded, a channel header that Close never
// rewrites. The non-resolving path reads two more, both inside
// lookupClosedErr: p.err, and — only when that err records a
// COMPLETED Close, which is exactly when Close has already cloned
// the bytes into host memory — p.pluginpath. Every other value's
// pluginpath stays unread, which is why the error it gets back
// names no path.
//
// lookup serialises against Plugin.Close via pluginsMu: closePlugin
// mutates p.syms (nils it), p.pluginpath (clones it off the plugin's
// RODATA before FreeLibrary unmaps those bytes) and p.err while
// holding the mutex, so an unlocked read here could race a concurrent
// Close — torn map access at best, a pluginpath dangling into
// unmapped memory in the error path at worst.
//
// pluginsMu is NOT what orders these reads against the OTHER writer
// of p.syms: open publishes p.syms, p.funcDescriptors and p.handle
// after releasing the mutex, and what makes that safe is the
// close(p.loaded) happens-before edge — every *Plugin open hands out
// has passed close(p.loaded) (directly, or via <-p.loaded on the
// cached path) before any caller can Lookup on it. Any future path
// that hands out a *Plugin without waiting on p.loaded re-opens this
// race even with the lock below intact.
//
// Lookup never runs with pluginsMu already held (open and closePlugin
// do not call it), so taking the mutex here cannot deadlock. The
// mutex is also what makes the cache walk in liveOrigin safe, and
// what guarantees the entry it returns cannot be unmapped before
// its syms and pluginpath are read.
func lookup(p *Plugin, symName string) (Symbol, error) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	// Read syms and pluginpath off the cached live entry, never off
	// p itself: p may be a struct copy whose fields outlived the
	// original's Close (trendvidia/go#64). See liveOrigin.
	origin := liveOrigin(p)
	if origin == nil {
		return nil, lookupClosedErr(p)
	}
	if s := origin.syms[symName]; s != nil {
		return s, nil
	}
	return nil, errors.New("plugin: symbol " + symName + " not found in plugin " + origin.pluginpath)
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
// imageExtent returns the base address and byte length of a loaded
// module's image, or (0, 0) if the headers do not read as a 64-bit PE.
//
// An HMODULE IS the image base on Windows, and the loader's own notion
// of how much address space the image occupies is SizeOfImage in the
// optional header. Both are read from the MAPPED image, so this must
// run before FreeLibrary.
//
// Returning (0, 0) rather than guessing is deliberate: reserveUnmapped
// would otherwise reserve a range that is not the image's, and a wrong
// reservation is worse than none.
func imageExtent(handle uintptr) (base, size uintptr) {
	if handle == 0 {
		return 0, 0
	}
	read16 := func(off uintptr) uint16 {
		return *(*uint16)(unsafe.Pointer(handle + off))
	}
	read32 := func(off uintptr) uint32 {
		return *(*uint32)(unsafe.Pointer(handle + off))
	}
	// IMAGE_DOS_HEADER.e_magic == "MZ", e_lfanew at 0x3c.
	if read16(0) != 0x5a4d {
		return 0, 0
	}
	nt := uintptr(read32(0x3c))
	// IMAGE_NT_HEADERS.Signature == "PE\0\0", then a 20-byte
	// IMAGE_FILE_HEADER, then the optional header.
	if read32(nt) != 0x00004550 {
		return 0, 0
	}
	opt := nt + 4 + 20
	// IMAGE_NT_OPTIONAL_HDR64_MAGIC. Only 64-bit images are
	// supported here, and SizeOfImage sits at a different offset in
	// the 32-bit layout, so refuse rather than misread it.
	if read16(opt) != 0x20b {
		return 0, 0
	}
	sz := uintptr(read32(opt + 56)) // OptionalHeader.SizeOfImage
	if sz == 0 {
		return 0, 0
	}
	return handle, sz
}

// reserveUnmapped reserves the address range a just-unloaded plugin
// occupied, so the loader cannot hand it to the NEXT plugin
// (trendvidia/go#122).
//
// Without it, a host pointer held across Close does not dangle in any
// detectable way: Windows reuses the freed base for the next image
// despite ASLR (measured on amd64 and arm64), so the pointer silently
// addresses whatever the new image put at that offset. During a plugin
// UPGRADE that is a different variable — measured, a host writing what
// it believed was one exported field set an unrelated one. Reserving
// the range turns that into an access violation at the point of use.
//
// MEM_RESERVE only: no memory is committed, so this costs address
// space and nothing else. Reserved-but-uncommitted pages fault on any
// access, which is exactly the desired behaviour for a stale pointer.
//
// Best-effort by design. The reservation fails when the range is not
// free, and the honest reading of that is "the image did not actually
// go away" — FreeLibrary decrements a refcount, so another loader
// reference (or a dependency holding it) keeps the image mapped. In
// that case the old pointers remain valid and no aliasing is possible,
// so there is nothing to protect against. Failing Close over it would
// turn a benign case into an error.
func reserveUnmapped(base, size uintptr) {
	if base == 0 || size == 0 {
		return
	}
	const (
		memReserve   = 0x2000
		pageNoAccess = 0x01
	)
	procVirtualAlloc.Call(base, size, memReserve, pageNoAccess)
}

// unloadPlugin ignores the runtime-supplied range: an HMODULE gives
// the exact image extent, headers included, where the module bounds
// stop at text..end.
func unloadPlugin(p *Plugin, _, _ uintptr) error {
	if p.handle == 0 {
		return nil
	}
	// Read the image extent while it is still mapped; after
	// FreeLibrary the headers are gone (trendvidia/go#122).
	base, size := imageExtent(p.handle)
	if err := syscall.FreeLibrary(syscall.Handle(p.handle)); err != nil {
		return err
	}
	reserveUnmapped(base, size)
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
// The runtime's push of this symbol is deliberately left unconfined in
// cmd/link/internal/loader.blockedLinknames, so any package linked into
// a host may pull it: the rejectchained fixture pulls it from a main
// package, whose recorded path the linker cannot name. The reasoning
// and the ways out are at those entries (trendvidia/go#86).
//
//go:linkname chainTail
func chainTail() uintptr

// chainGrew is defined in package runtime via go:linkname. It reports
// whether a node has been chained after prevTail — open's pre-
// LoadLibrary chainTail snapshot — by reading only prevTail's next
// link, which is a field of a module this host may read; nothing of
// the appended node itself is inspected.
//
// Deliberately unconfined in blockedLinknames, as chainTail is
// (trendvidia/go#86): this is the arbitrary-address read that pairs
// with rejectChainedModule's write.
//
//go:linkname chainGrew
func chainGrew(prevTail uintptr) bool

// testHookBetweenFenceStages, when set, runs after stage 1 has judged
// the plugin's file bytes and before LoadLibrary maps them, with
// pluginsMu held. It exists so a test fixture can replace the file in
// that window: a file the loader maps that differs from the one stage
// 1 read is the only way stage 2 can reject without a second
// toolchain generation, so without the hook open's post-load
// rejection — its rejectChainedModule call, negative-cache write and
// error text — is unreachable by any test (trendvidia/go#48). The
// cmd/cgo/internal/testplugin rejectchained fixture sets it via
// go:linkname; production code never does.
//
// DECISION (trendvidia/go#78, 2026-08-27): the push linkname stays, and
// this comment is the written rule it establishes.
//
// A //go:linkname on a definition is a PUSH -- either form of the
// directive, since the compiler sets sym.Linkname from both and marks
// the def obj.AttrLinkname whenever it is set -- and a push on the def
// is what makes a symbol legally pull-linknameable from any other
// package under the default -checklinkname=1
// (cmd/link/internal/loader.checkLinkname). It is not the only way
// through that check, which allows osym.IsLinkname() ||
// osym.ABIWrapper(); ABI wrappers attend assembly, package plugin has
// none, and TestPluginPushLinknameSurface hard-fails if it gains any,
// which is what keeps this rule's set the whole set.
//
// So in every production build, code linked into a host can pull this
// var and run arbitrary code inside open, with pluginsMu held, between
// stage 1 judging the file's bytes and LoadLibrary mapping them -- the
// TOCTOU window the two-stage fence exists to close.
//
// Accepted, because code that can be linked into the host is already
// inside the trust boundary: it could call syscall.LoadLibrary directly
// and skip the fence altogether. The hook widens no boundary; it makes a
// path that already existed look supported. Weighed and kept rather than
// overlooked.
//
// The rule: this is the ONLY symbol package plugin itself pushes. Every
// other go:linkname here is a bodyless declaration -- a PULL of a symbol
// the runtime defines and chose to export to us, which exposes nothing
// outward. TestPluginPushLinknameSurface pins that set; adding a second
// push is a decision, not a detail, and that test is where it will be
// noticed.
//
// It is not the whole plugin.* surface, and this decision does not cover
// the rest of it: package runtime pushes nine of its own definitions in
// under plugin.* names, none of them confined by
// cmd/link/internal/loader.blockedLinknames, so those are reachable from
// any package too. trendvidia/go#86 is open on whether they should be;
// TestRuntimePushesIntoPluginNamespace pins that set meanwhile.
//
//go:linkname testHookBetweenFenceStages
var testHookBetweenFenceStages func(pluginPath string)

// callFenceHook runs an installed testHookBetweenFenceStages. open
// holds pluginsMu across the call with no deferred unlock, so a panic
// (or a runtime.Goexit) inside the hook would wedge every later Open,
// Lookup and Close in the process — the same hazard loadedModulePath
// resolves its procs explicitly to avoid. Release the lock on the way
// out so a fixture bug surfaces as its own panic, with its own stack,
// instead of as a deadlock in an unrelated goroutine.
func callFenceHook(hook func(string), pluginPath string) {
	returned := false
	defer func() {
		if !returned {
			pluginsMu.Unlock()
		}
	}()
	hook(pluginPath)
	returned = true
}

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
// Deliberately unconfined in blockedLinknames, as chainTail is
// (trendvidia/go#86), and the sharpest of the three: it writes through
// the address it is handed. Any package linked into a host may pull it
// and call it with an address of its own choosing until that gap is
// closed.
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
	// Resolve explicitly: LazyProc.Call panics when the DLL or the
	// procedure cannot be found, and open holds pluginsMu across this
	// call with no deferred unlock, so a panic here would wedge every
	// later Open, Lookup and Close in the process. "Cannot be
	// determined" is a documented answer; a deadlock is not.
	if err := procGetModuleFileNameW.Find(); err != nil {
		return ""
	}
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
// forward slashes, the \\?\ literal-device prefix, and 8.3 short-name
// components (RUNNER~1), which the loader expands to their long form
// when it records where a module lives. A mismatch is therefore a
// genuinely different module. Exotic-but-equivalent spellings this
// comparison does not model (junctions, \\?\UNC\) degrade to a
// rejection whose message names both spellings — loud and diagnosable,
// never a silent alias.
func sameModulePath(a, b string) bool {
	norm := func(p string) string {
		p = longPathName(p)
		p = strings.ReplaceAll(p, "/", "\\")
		p = strings.TrimPrefix(p, `\\?\`)
		return p
	}
	return strings.EqualFold(norm(a), norm(b))
}

// longPathName expands any 8.3 short-name components of p through
// GetLongPathNameW, which is the same expansion the loader applies when
// it records a module's path. A path the call cannot resolve (a file
// that does not exist, an unrepresentable string) is returned unchanged
// so the caller still compares the spelling it was given rather than
// nothing.
func longPathName(p string) string {
	// Resolve explicitly instead of calling syscall.GetLongPathName:
	// that wrapper reaches the procedure through LazyProc.Addr, which
	// panics when it cannot be found, and open holds pluginsMu across
	// the sameModulePath call below with no deferred unlock, so the
	// panic would wedge every later Open, Lookup and Close in the
	// process — the same hazard loadedModulePath resolves explicitly to
	// avoid. "Could not expand" is an answer this function documents; a
	// deadlock is not.
	if err := procGetLongPathNameW.Find(); err != nil {
		return p
	}
	up, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	for size := uint32(syscall.MAX_PATH); size <= 65536; size *= 2 {
		buf := make([]uint16, size)
		// LazyProc.Call is //go:uintptrescapes, so up and buf stay
		// alive across it. The DWORD return needs narrowing before it
		// is used as a length, exactly as loadedModulePath narrows
		// GetModuleFileNameW's: the Windows x64 ABI leaves the upper
		// half of RAX undefined for one, and an unnarrowed value would
		// index out of range under pluginsMu.
		r0, _, _ := procGetLongPathNameW.Call(uintptr(unsafe.Pointer(up)), uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
		n := uint32(r0)
		if n == 0 {
			return p
		}
		if n >= size {
			// Truncated: n is the size the result needs, including
			// its terminator, so the next doubling covers it.
			continue
		}
		return syscall.UTF16ToString(buf[:n])
	}
	return p
}

// doInit is defined in package runtime.
//
//go:linkname doInit runtime.doInit
func doInit(t []*initTask)

type initTask struct {
	// fields defined in runtime.initTask. We only handle pointers to an initTask
	// in this package, so the contents are irrelevant.
}
