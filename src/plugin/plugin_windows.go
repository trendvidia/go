// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"errors"
	"path/filepath"
	"syscall"
	"unsafe"
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

	// A Go plugin DLL chains its moduledata into this process from its
	// DllMain during LoadLibrary (addmoduledata runs while this
	// function holds pluginsMu). If the chain did not grow, the DLL
	// never initialized HERE — e.g. it was linked against another
	// program's import library and PE bound its imports by module name
	// elsewhere (observed building the peerlock fixture,
	// trendvidia/go#24/#31). Refuse before validating:
	// validatePkghashes identifies the candidate as the newly chained
	// module, and on a chain that did not grow it would take the last
	// healthy peer for one — silently passing a divergent build, or
	// marking/detaching that peer's moduledata while the FreeLibrary
	// below unmaps only the offender. No syncBeforeUnmap is needed:
	// the module never entered activeModules, so no GC root references
	// this image.
	if prevTail := chainTail(); chainTail() == prevTail {
		const noModData = "plugin DLL registered no Go module data in this process" +
			" (its runtime imports may be bound to another program's image)"
		syscall.FreeLibrary(h)
		plugins[pluginPath] = &Plugin{err: noModData}
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + noModData)
	}

	if proc, err := syscall.GetProcAddress(h, "go:plugin.pkghashes"); err == nil {
		if errstr, rejectedPath := validatePkghashes(uintptr(proc)); errstr != "" {
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

// doInit is defined in package runtime.
//
//go:linkname doInit runtime.doInit
func doInit(t []*initTask)

type initTask struct {
	// fields defined in runtime.initTask. We only handle pointers to an initTask
	// in this package, so the contents are irrelevant.
}
