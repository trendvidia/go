// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && cgo) || (darwin && cgo) || (freebsd && cgo)

package plugin

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <sys/mman.h>
#include <unistd.h>
#include <limits.h>
#include <stdlib.h>
#include <stdint.h>

#include <stdio.h>

static uintptr_t pluginOpen(const char* path, char** err) {
	void* h = dlopen(path, RTLD_NOW|RTLD_GLOBAL);
	if (h == NULL) {
		*err = (char*)dlerror();
	}
	return (uintptr_t)h;
}

static void* pluginLookup(uintptr_t h, const char* name, char** err) {
	void* r = dlsym((void*)h, name);
	if (r == NULL) {
		*err = (char*)dlerror();
	}
	return r;
}

// pluginReserveRange takes the address range a just-unloaded plugin
// occupied, so the dynamic linker cannot place the NEXT plugin over it
// and a stale host pointer faults instead of silently addressing the
// new image (trendvidia/go-archive#123).
//
// Deliberately WITHOUT MAP_FIXED. MAP_FIXED would succeed by unmapping
// whatever is already there, and "already there" is exactly the case
// this must not touch: dlclose decrements a refcount, so another
// reference (the host's own dlopen, a dependency) can keep the image
// mapped. Windows gets that safety for free — VirtualAlloc simply
// fails on an occupied range — and here it has to be built.
//
// Without MAP_FIXED the address is a hint: the kernel honours it when
// the range is free and places the mapping elsewhere when it is not.
// Landing elsewhere means the range was not free, so the mapping is
// released and nothing is reserved, which is the correct outcome for
// the still-mapped case.
//
// PROT_NONE: no memory is committed and any access faults, which is
// what a stale pointer should do.
static void pluginReserveRange(uintptr_t base, uintptr_t size) {
	if (base == 0 || size == 0) {
		return;
	}
	// mmap only honours a PAGE-ALIGNED hint. The module bounds are
	// not aligned (text starts partway into a page), so round the
	// range outward to whole pages first.
	uintptr_t ps = (uintptr_t)sysconf(_SC_PAGESIZE);
	if (ps == 0) {
		return;
	}
	uintptr_t end = (base + size + ps - 1) & ~(ps - 1);
	uintptr_t start = base & ~(ps - 1);
	void* got = mmap((void*)start, (size_t)(end - start), PROT_NONE,
		MAP_PRIVATE|MAP_ANON, -1, 0);
	if (got == MAP_FAILED) {
		return;
	}
	if (got != (void*)start) {
		munmap(got, (size_t)(end - start));
	}
}

// pluginOpenCopy loads a fresh copy of path. A closed plugin's image
// stays mapped (pluginCloseHandle does not dlclose), and dlopen of a
// file that is still loaded returns that image without running its
// initializers again, so a re-Open must load a file the loader has not
// seen: a copy with its own inode. The copy is unlinked once loaded;
// the mapping keeps it alive.
static uintptr_t pluginOpenCopy(const char* path, char** err) {
	static char tmpl[PATH_MAX];
	const char* dir = getenv("TMPDIR");
	if (dir == NULL || dir[0] == 0) {
		dir = "/tmp";
	}
	snprintf(tmpl, sizeof tmpl, "%s/goplugin-XXXXXX", dir);
	int out = mkstemp(tmpl);
	if (out < 0) {
		*err = "plugin: cannot create a copy to re-open";
		return 0;
	}
	FILE* in = fopen(path, "rb");
	if (in == NULL) {
		close(out);
		unlink(tmpl);
		*err = "plugin: cannot read the plugin to re-open it";
		return 0;
	}
	char buf[65536];
	size_t n;
	int ok = 1;
	while ((n = fread(buf, 1, sizeof buf, in)) > 0) {
		if (write(out, buf, n) != (ssize_t)n) {
			ok = 0;
			break;
		}
	}
	fclose(in);
	close(out);
	if (!ok) {
		unlink(tmpl);
		*err = "plugin: cannot write a copy to re-open";
		return 0;
	}
	uintptr_t h = pluginOpen(tmpl, err);
	unlink(tmpl);
	return h;
}

static int pluginCloseHandle(uintptr_t h, char** err) {
	// dlclose returns 0 on success, non-zero on failure with the
	// reason retrievable via dlerror. After a successful return the
	// .so's mapping refcount is decremented; the OS unmaps once it
	// reaches zero. v2's Plugin.Close rewrites every registered
	// descriptor to the runtime panic stub before this call, so a
	// goroutine holding a Lookup-obtained func value lands its call
	// in the stub (mapped in the host) rather than in what we're
	// about to unmap. References obtained any other way — closures
	// returned by calls into the plugin, pointers to variables —
	// are not registered anywhere Close can reach, and using one
	// after this point faults unrecoverably.
	if (dlclose((void*)h) != 0) {
		*err = (char*)dlerror();
		return 1;
	}
	return 0;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

func open(name string) (*Plugin, error) {
	cPath := make([]byte, C.PATH_MAX+1)
	cRelName := make([]byte, len(name)+1)
	copy(cRelName, name)
	if C.realpath(
		(*C.char)(unsafe.Pointer(&cRelName[0])),
		(*C.char)(unsafe.Pointer(&cPath[0]))) == nil {
		return nil, errors.New(`plugin.Open("` + name + `"): realpath failed`)
	}

	filepath := C.GoString((*C.char)(unsafe.Pointer(&cPath[0])))

	pluginsMu.Lock()
	if p := plugins[filepath]; p != nil {
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
	var cErr *C.char
	var h C.uintptr_t
	if retired[filepath] {
		h = C.pluginOpenCopy((*C.char)(unsafe.Pointer(&cPath[0])), &cErr)
	} else {
		h = C.pluginOpen((*C.char)(unsafe.Pointer(&cPath[0])), &cErr)
	}
	if h == 0 {
		pluginsMu.Unlock()
		return nil, errors.New(`plugin.Open("` + name + `"): ` + C.GoString(cErr))
	}
	// TODO(crawshaw): look for plugin note, confirm it is a Go plugin
	// and it was built with the correct toolchain.
	if len(name) > 3 && name[len(name)-3:] == ".so" {
		name = name[:len(name)-3]
	}
	if plugins == nil {
		plugins = make(map[string]*Plugin)
	}
	pluginpath, syms, initTasks, errstr := lastmoduleinit()
	if errstr != "" {
		plugins[filepath] = &Plugin{
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
		handle:     uintptr(h),
	}
	plugins[filepath] = p
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
		cname := make([]byte, len(fullName)+1)
		copy(cname, fullName)

		dlp := C.pluginLookup(h, (*C.char)(unsafe.Pointer(&cname[0])), &cErr)
		if dlp == nil {
			// p is already cached with loaded unclosed; a bare return
			// strands every future Open of this path on <-p.loaded
			// forever (and handed the caller no *Plugin to Close).
			// Mark the entry failed and wake waiters; subsequent
			// Opens report "(previous failure)". The .so stays mapped
			// and the handle unreleased — matching this file's other
			// failure classes, which also retain the mapping — and
			// nothing may dlclose it: no *Plugin with a live handle
			// escaped.
			errStr := "could not find symbol " + symName + ": " + C.GoString(cErr)
			pluginsMu.Lock()
			p.err = errStr
			close(p.loaded)
			pluginsMu.Unlock()
			return nil, errors.New(`plugin.Open("` + name + `"): ` + errStr)
		}
		valp := (*[2]unsafe.Pointer)(unsafe.Pointer(&sym))
		if isFunc {
			// A Go func value is a 1-pointer struct whose data
			// pointer points at a closure descriptor; calling
			// the value loads descriptor[0] and indirects to it.
			// Allocate that descriptor explicitly here (instead
			// of the historic &p escape trick) so we have a
			// stable reference to give to Plugin.Close — at
			// Close, the runtime overwrites *holder with the
			// pluginClosedFuncStub PC, converting any held call
			// through this Lookup func value into a clean panic.
			// Runtime-created closures returned by calls into the
			// plugin carry no such holder and stay unprotected.
			holder := new(uintptr)
			*holder = uintptr(dlp)
			(*valp)[1] = unsafe.Pointer(holder)
			funcDescriptors = append(funcDescriptors, holder)
		} else {
			(*valp)[1] = dlp
		}
		// we can't add to syms during iteration as we'll end up processing
		// some symbols twice with the inability to tell if the symbol is a function
		updatedSyms[symName] = sym
	}
	p.syms = updatedSyms
	p.funcDescriptors = funcDescriptors

	close(p.loaded)
	return p, nil
}

// lookup resolves p to the live cache entry (liveOrigin) and reads
// syms/pluginpath off THAT entry, never off p: p may be a struct
// copy whose fields outlived the original's Close, where the syms
// map's keys and the pluginpath header both point into the unmapped
// image (trendvidia/go-archive#64). On the resolving path the only field
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
// RODATA before dlclose unmaps those bytes) and p.err while holding
// the mutex, so an unlocked read here could race a concurrent Close —
// torn map access at best, a pluginpath dangling into unmapped
// memory in the error path at worst.
//
// pluginsMu is NOT what orders these reads against the OTHER writer
// of p.syms: open publishes p.syms and p.funcDescriptors after
// releasing the mutex, and what makes that safe is the close(p.loaded)
// happens-before edge — every *Plugin open hands out has passed
// close(p.loaded) (directly, or via <-p.loaded on the cached path)
// before any caller can Lookup on it. Any future path that hands out
// a *Plugin without waiting on p.loaded re-opens this race even with
// the lock below intact.
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
	// original's Close (trendvidia/go-archive#64). See liveOrigin.
	origin := liveOrigin(p)
	if origin == nil {
		return nil, lookupClosedErr(p)
	}
	if s := origin.syms[symName]; s != nil {
		return s, nil
	}
	return nil, errors.New("plugin: symbol " + symName + " not found in plugin " + origin.pluginpath)
}

// unloadPlugin is the platform-side phase 2 of Plugin.Close: it
// dlcloses the OS handle so the .so's mapping refcount drops and
// the kernel may unmap the text/data backing pages.
//
// The Plugin's runtime state has already been torn down by phase 1
// (descriptor rewrite + module unlink) when this is called. A zero
// handle (failed-load placeholder, or an entry that already had
// Close run) is a no-op so callers can be conservative.
//
// Cache cleanup: the plugins[filepath] entry is also removed so a
// future plugin.Open of the same path obtains a fresh dlopen (and a
// fresh moduledata) instead of latching onto the closed handle.
// Without this step, Close would unmap but the next Open would
// short-circuit on the cached closed *Plugin.
//
// Caller (closePlugin) holds pluginsMu.
// retired holds the paths whose plugin was closed. Their image stays
// mapped, so Open loads a copy of the file instead. Guarded by
// pluginsMu.
var retired = map[string]bool{}

func unloadPlugin(p *Plugin, base, size uintptr) error {
	if p.handle == 0 {
		return nil
	}
	// PROTOTYPE (trendvidia/go#79, option 3): the image stays mapped.
	// A runtime thread can be asleep in the plugin's own copy of the
	// scheduler, which the plugin's code calls directly, and it returns
	// into the image when it wakes: unmapping the image faults it.
	// Keeping the image also means nothing else can be placed in its
	// range, so pluginReserveRange is not needed.
	for path, q := range plugins {
		if q == p {
			delete(plugins, path)
			retired[path] = true
			break
		}
	}
	return nil
}

// lastmoduleinit is defined in package runtime.
func lastmoduleinit() (pluginpath string, syms map[string]any, inittasks []*initTask, errstr string)

// doInit is defined in package runtime.
//
//go:linkname doInit runtime.doInit
func doInit(t []*initTask)

type initTask struct {
	// fields defined in runtime.initTask. We only handle pointers to an initTask
	// in this package, so the contents are irrelevant.
}
