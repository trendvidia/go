// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"errors"
	"path/filepath"
	"sync"
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
		pluginsMu.Unlock()
		if p.err != "" {
			return nil, errors.New(`plugin.Open("` + name + `"): ` + p.err + ` (previous failure)`)
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

	if proc, err := syscall.GetProcAddress(h, "go:plugin.pkghashes"); err == nil {
		if errstr := validatePkghashes(uintptr(proc)); errstr != "" {
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + errstr)
		}
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
	for symName, sym := range syms {
		isFunc := symName[0] == '.'
		if isFunc {
			delete(syms, symName)
			symName = symName[1:]
		}

		fullName := pluginpath + "." + symName
		proc, procErr := syscall.GetProcAddress(h, fullName)
		if procErr != nil {
			return nil, errors.New(`plugin.Open("` + name + `"): could not find symbol ` + symName + `: ` + procErr.Error())
		}
		valp := (*[2]unsafe.Pointer)(unsafe.Pointer(&sym))
		if isFunc {
			(*valp)[1] = unsafe.Pointer(&proc)
		} else {
			(*valp)[1] = unsafe.Pointer(proc)
		}
		// we can't add to syms during iteration as we'll end up processing
		// some symbols twice with the inability to tell if the symbol is a function
		updatedSyms[symName] = sym
	}
	p.syms = updatedSyms

	close(p.loaded)
	return p, nil
}

func lookup(p *Plugin, symName string) (Symbol, error) {
	if s := p.syms[symName]; s != nil {
		return s, nil
	}
	return nil, errors.New("plugin: symbol " + symName + " not found in plugin " + p.pluginpath)
}

// unloadPlugin is the Windows counterpart of plugin_dlopen.go's
// dlclose-based phase 2. The current Windows port does not yet
// support per-plugin import libraries for multi-host fixtures, so the
// v2/L3 tests that exercise Plugin.Close are skipped on Windows and
// this path is unreachable from those tests. The function is defined
// so closePlugin compiles uniformly across platforms.
func unloadPlugin(p *Plugin) error {
	return nil
}

var (
	pluginsMu sync.Mutex
	plugins   map[string]*Plugin
)

// lastmoduleinit is defined in package runtime via go:linkname.
//
//go:linkname lastmoduleinit
func lastmoduleinit() (pluginpath string, syms map[string]any, inittasks []*initTask, errstr string)

// validatePkghashes is defined in runtime/plugin.go via go:linkname.
// addr is the address (from GetProcAddress) of the plugin's
// go:plugin.pkghashes export, i.e. a slice header for the plugin's
// pkghashes carrier. Returns "" on success or an error message
// describing the first mismatched package.
//
//go:linkname validatePkghashes
func validatePkghashes(addr uintptr) string

// doInit is defined in package runtime.
//
//go:linkname doInit runtime.doInit
func doInit(t []*initTask)

type initTask struct {
	// fields defined in runtime.initTask. We only handle pointers to an initTask
	// in this package, so the contents are irrelevant.
}
