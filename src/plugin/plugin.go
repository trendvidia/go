// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package plugin implements loading and symbol resolution of Go plugins.
//
// A plugin is a Go main package with exported functions and variables that
// has been built with:
//
//	go build -buildmode=plugin
//
// When a plugin is first opened, the init functions of all packages not
// already part of the program are called. The main function is not run.
// A plugin is only initialized once; [Plugin.Close] can later unload it
// (fork extension).
//
// # Warnings
//
// The ability to dynamically load parts of an application during
// execution, perhaps based on user-defined configuration, may be a
// useful building block in some designs. In particular, because
// applications and dynamically loaded functions can share data
// structures directly, plugins may enable very high-performance
// integration of separate parts.
//
// However, the plugin mechanism has many significant drawbacks that
// should be considered carefully during the design. For example:
//
//   - Plugins are currently supported only on Linux, FreeBSD, macOS,
//     and Windows (amd64, arm64), making them unsuitable for
//     applications intended to be portable.
//
//   - Plugins are poorly supported by the Go race detector. Even simple
//     race conditions may not be automatically detected. See
//     https://go.dev/issue/24245 for more information.
//
//   - Applications that use plugins may require careful configuration
//     to ensure that the various parts of the program be made available
//     in the correct location in the file system (or container image).
//     By contrast, deploying an application consisting of a single static
//     executable is straightforward.
//
//   - Reasoning about program initialization is more difficult when
//     some packages may not be initialized until long after the
//     application has started running.
//
//   - Bugs in applications that load plugins could be exploited by
//     an attacker to load dangerous or untrusted libraries.
//
//   - Runtime crashes are likely to occur unless all parts of the
//     program (the application and all its plugins) are compiled
//     using exactly the same version of the toolchain, the same build
//     tags, and the same values of certain flags and environment
//     variables.
//
//   - Similar crashing problems are likely to arise unless all common
//     dependencies of the application and its plugins are built from
//     exactly the same source code.
//
//   - Together, these restrictions mean that, in practice, the
//     application and its plugins must all be built together by a
//     single person or component of a system. In that case, it may
//     be simpler for that person or component to generate Go source
//     files that blank-import the desired set of plugins and then
//     compile a static executable in the usual way.
//
// For these reasons, many users decide that traditional interprocess
// communication (IPC) mechanisms such as sockets, pipes, remote
// procedure call (RPC), shared memory mappings, or file system
// operations may be more suitable despite the performance overheads.
//
// # Authoring a plugin main package (fork note)
//
// A plugin's main package may declare a `func main`; -buildmode=plugin
// accepts it and does not run it (only package init functions run at
// plugin load). This lets a single package satisfy both a plugin build
// and a stock default `go build ./...` — which requires `func main` to
// link — without a per-plugin build-tag stub. In other words:
//
//	package main
//
//	func Exported() { /* ... */ }
//
//	func main() {} // ignored by -buildmode=plugin; keeps `go build ./...` and go vet green
//
// is a supported, canonical pattern; you do not need a `//go:build`
// stub to drop `func main` out of the plugin build. See trendvidia/go#6.
//
// # Version locking spans all shared packages (fork note)
//
// [Open] rejects a plugin whose build of any package linked into both
// the host and the plugin differs from the host's build of that same
// package — not merely a single toolchain/compiler version. The same
// comparison runs against every already-loaded plugin for packages the
// host does not link (trendvidia/go#24), so two plugins built from
// different sources of one dependency cannot coexist silently: the
// first loader owns the package and later divergent builds are
// refused. The lock surface is every shared package: the runtime,
// google.golang.org/protobuf, and any other dependency the host and
// plugin both link. A standalone plugin build therefore loads only
// when the host resolves an identical dependency graph.
//
// The practical consequence: build the host and the plugin against the
// same resolved module graph. If the host resolves dependencies through
// a go.work overlay or a `replace` directive, the plugin must resolve
// them the same way; the reliable recipe is to build both with
// GOWORK=off against pinned go.mod versions (or both under the same
// workspace) so their per-package build hashes match.
//
// When a mismatch is detected, [Open] reports the offending package and
// both build hashes, naming the already-loaded side, for example:
//
//	plugin was built with a different version of package google.golang.org/protobuf/internal/pragma (plugin build hash 1a2b…, host build hash 9f8e…)
//	plugin was built with a different version of package example.com/shared (plugin build hash 1a2b…, peer /plugins/a build hash 9f8e…)
//
// so the diverging package — and which loaded module holds the stale
// build — is identifiable without decoding the raw loader message. See
// trendvidia/go#7.
//
// # Toolchain ABI fence (fork note)
//
// On Windows, [Open] verifies a go:plugin.abi marker carried by every
// plugin DLL — an export identifying the toolchain generation that
// built it — twice. Before anything is mapped, the candidate file's
// export table is parsed directly, so a binary built by another fork
// or toolchain generation (whose runtime data structures may have a
// different layout) is rejected with a clean error before any of its
// code runs, instead of being loaded and faulting. What the file bytes
// cannot decide — WHICH module LoadLibrary will return — is settled
// after the load, by two checks that cover its two failure modes.
// When the loader hands back an already-mapped module instead of a
// fresh map (detected because the runtime chain did not grow), the
// handle's loader-recorded path is compared with the opened path, and
// a request that resolved to a module from another directory is
// refused with an error naming both paths rather than silently aliased;
// nothing chained in this case, so the handle is freed. Whatever WAS
// freshly mapped also has the marker re-read through GetProcAddress,
// catching a candidate file swapped in between the two reads; by then
// the module's DllMain has chained its moduledata, so that rejection
// refuses the load but leaves the image mapped — detached from the
// runtime chain, so walks starting afterwards do not read it — because
// unloading safely needs the stop-the-world barrier Close takes. A
// walk already under way when the load is refused is not covered: that
// window opens inside the loader, before [Open] regains control. The
// generation number's bump rules live in internal/pluginabi.
// POSIX needs no equivalent: the dynamic linker resolves symbols
// across modules at load time, so incompatible binaries already fail
// there.
package plugin

import (
	"sync"
	_ "unsafe" // for go:linkname
)

// pluginsMu serialises every mutation of plugin state shared across
// goroutines: the plugins path cache below, and — critically — the
// runtime's moduledata chain. The chain is APPENDED to by
// runtime.addmoduledata, which runs lock-free from a module
// constructor inside dlopen/LoadLibrary while open() holds
// pluginsMu; it is UNLINKED by Plugin.Close (runtime pluginClose).
// closePlugin therefore holds pluginsMu for its whole body so the
// unlink (and its lastmoduledatap fixup) can never interleave with
// a concurrent Open's append. Lock order: pluginsMu, then runtime
// locks (canonicalSymsLock, itabLock) — the same order open()'s
// lastmoduleinit establishes.
var (
	pluginsMu sync.Mutex
	plugins   map[string]*Plugin
)

// Plugin is a loaded Go plugin.
type Plugin struct {
	pluginpath string
	err        string        // set if plugin failed to load
	loaded     chan struct{} // closed when loaded
	syms       map[string]any
	// funcDescriptors are heap-allocated 1-word holders, one per
	// function symbol returned by Lookup; each holds the dlsym'd
	// code address and is the data pointer of the user's func
	// value. At Close, the runtime overwrites *each holder with
	// the address of runtime.pluginClosedFuncStub so a held call
	// through that Lookup value panics with a recognisable
	// message instead of jumping into what dlclose has just
	// unmapped. The protection covers Lookup results only; see
	// [Plugin.Close] for the unprotected reference classes.
	funcDescriptors []*uintptr
	// handle is the platform dlopen handle (or 0 in stub builds /
	// failed-load entries). Plugin.Close passes this to the
	// platform unloadPlugin path which calls dlclose, releasing
	// the OS-level mapping after the runtime side has rewritten
	// every Lookup-obtained descriptor to a panic stub. Handles are not
	// reused across re-opens: each successful Open obtains a fresh
	// handle from dlopen.
	handle uintptr
}

// Open opens a Go plugin.
// If a path has already been opened, then the existing *[Plugin] is returned.
// It is safe for concurrent use by multiple goroutines.
func Open(path string) (*Plugin, error) {
	return open(path)
}

// Lookup searches for a symbol named symName in plugin p.
// A symbol is any exported variable or function.
// It reports an error if the symbol is not found.
// It is safe for concurrent use by multiple goroutines.
func (p *Plugin) Lookup(symName string) (Symbol, error) {
	return lookup(p, symName)
}

// Close marks the plugin closed and releases its internal runtime
// state. Returns an error and leaves the plugin loaded if any other
// plugin currently holds canonicalised references into this plugin
// (see [Plugin.Dependents]); the caller must Close those plugins
// first.
//
// After a successful Close, [Plugin.Lookup] returns an error. The
// protection Close provides depends on how a reference into the
// plugin was obtained:
//
//   - Function values returned by [Plugin.Lookup] remain safe to
//     call: each call panics with a recoverable "plugin: function
//     value invoked after Plugin.Close" error instead of executing
//     plugin code.
//
//   - Any other reference has no protection: closures returned by
//     calls into the plugin, function values reached through such
//     closures, and pointers to the plugin's variables all point at
//     memory the OS unmaps during Close. Using one after Close
//     faults unrecoverably (the process dies even under defer/
//     recover). If the same path is re-opened afterwards, stale
//     pointers may additionally alias the new instance, because the
//     loader can map it at the same addresses.
//
// A call that was already in flight when Close began may still be
// executing plugin code as its image unmaps; the application must
// ensure every goroutine has returned from calls into the plugin
// before invoking Close.
//
// Close reports an error, and unloads nothing, for a Plugin value
// that [Open] did not hand out: a zero plugin.Plugin{}, and equally
// a struct copy of an opened Plugin, since closing a copy would
// unmap the image while the original stayed live in Open's cache.
// A rejected value is not marked closed, so a repeated Close keeps
// reporting the same error. Close on a nil *Plugin is a no-op and
// returns nil.
func (p *Plugin) Close() error {
	return closePlugin(p)
}

// HasDependents reports whether any other plugin currently holds
// canonicalised references into p. For a Plugin returned by [Open],
// false means [Plugin.Close] will not be refused for dependents;
// Close rejects a Plugin that Open did not hand out whatever this
// reports, since such a value names no loaded plugin at all.
//
// A Plugin that Open did not hand out — a zero value, or a struct
// copy of an opened Plugin — reports false, as does one that has
// already been closed.
func (p *Plugin) HasDependents() bool {
	return pluginHasDependents(p.snapshotPath())
}

// Dependents returns the pluginpaths of plugins that currently
// reference exported symbols from p. The application must Close
// those plugins (or release their references) before Closing p.
//
// The returned slice is a snapshot; concurrent loads or closes may
// invalidate it, but the strings in it are owned by the caller and
// stay readable after the plugins they name are closed. The host
// program (the main executable) is omitted from the list since it
// does not Unload.
//
// A Plugin that Open did not hand out — a zero value, or a struct
// copy of an opened Plugin — returns nil, as does one that has
// already been closed.
func (p *Plugin) Dependents() []string {
	return pluginDependents(p.snapshotPath())
}

// snapshotPath returns a host-owned copy of p.pluginpath taken
// under pluginsMu, or "" for a value that names no loaded plugin.
//
// closePlugin rewrites the field under the mutex (it clones the
// bytes out of the plugin's RODATA before dlclose unmaps them), so
// an unlocked read races the write (trendvidia/go#36). Locking the
// read alone is not enough: the caller runs the runtime hook after
// the mutex is dropped, so a Close completing in that window leaves
// the returned header pointing into RODATA that dlclose has since
// unmapped, and runtime.findPluginModule's string comparison
// dereferences it as soon as a surviving module's pluginpath has
// the same length. Cloning inside the critical section detaches the
// value from the mapping, which keeps the runtime hook outside the
// critical section, so canonicalSymsLock is never taken while
// pluginsMu is held (lock order at pluginsMu's doc comment).
//
// The clone is what makes the identity check mandatory rather than
// tidy: it copies len(pluginpath) bytes unconditionally, so it
// dereferences a header the caller may no longer own. Only while p
// is in the plugins cache are those bytes guaranteed mapped —
// closePlugin holds pluginsMu from the guard through unloadPlugin's
// dlclose and cache removal, so a cached entry cannot be unmapped
// under us. Everything else (a zero value, a struct copy of an
// opened Plugin, an already-closed plugin unloadPlugin evicted)
// names no loaded plugin, and its pluginpath may point into an
// image dlclose has already unmapped; cloning that faults. ""
// is what findPluginModule refuses, so both accessors report no
// dependents for such a value instead of dying.
//
// Unlocking via defer so a nil-receiver panic here releases
// pluginsMu instead of wedging every later Open/Lookup/Close, as
// lookup does for the same reason.
func (p *Plugin) snapshotPath() string {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	if !openedByOpen(p) {
		return ""
	}
	return cloneString(p.pluginpath)
}

// pluginHasDependents, pluginDependents, and pluginClose are
// defined in package runtime and linked here via go:linkname.

//go:linkname pluginHasDependents
func pluginHasDependents(pluginpath string) bool

//go:linkname pluginDependents
func pluginDependents(pluginpath string) []string

//go:linkname pluginClose
func pluginClose(pluginpath string, funcDescriptors []*uintptr) (errstr string)

// syncBeforeUnmap is a runtime stop-the-world barrier taken between the
// moduledata unlink in pluginClose and unmapping the plugin image in
// unloadPlugin, so a GC mark worker cannot be scanning the plugin's
// data/bss roots when dlclose/FreeLibrary frees those pages. Defined in
// package runtime.
//
//go:linkname syncBeforeUnmap
func syncBeforeUnmap()

// closePlugin is the platform-independent half of Plugin.Close. It
// runs in three ordered phases:
//
//  1. Runtime side (pluginClose): rewrites the closure descriptors
//     the runtime can reach — the static ones emitted into the
//     plugin's .noptrdata (found via canonicalSyms) and the
//     per-Lookup heap holders threaded through funcDescriptors — to
//     runtime.pluginClosedFuncStub. Closures created at runtime
//     inside the plugin and returned to the host are registered
//     nowhere and stay unprotected (see [Plugin.Close]). Also
//     removes the module's canonicalSyms entries and unlinks
//     moduledata.
//
//  2. Platform side (unloadPlugin): dlclose / FreeLibraryEx. The OS
//     unmaps the plugin's text and data. Func values obtained via
//     [Lookup] remain safe to call: phase 1 already redirected their
//     descriptors at the panic stub. Any other reference class has
//     no protection and faults unrecoverably if used from here on.
//
//  3. Marker: clear syms/funcDescriptors and set err so subsequent
//     Lookup calls return the standard "symbol not found" error and
//     repeated Close calls are no-ops.
//
// The phase ordering is load-bearing: phase 1 reads and writes the
// plugin's still-mapped descriptor pages; doing it after phase 2
// would fault. Phase 2 cannot precede phase 1 for the same reason
// (descriptors gone, held callers would jump into unmapped text).
//
// Concurrent Close + held-call: an in-flight call started before
// phase 1 may dispatch through the original (still-valid) code
// pointer it loaded, then phase 2 unmaps the text out from under
// it. v2 does not STW for unload; the application must ensure no
// goroutine is mid-call into the plugin when it invokes Close.
func closePlugin(p *Plugin) error {
	if p == nil {
		return nil
	}
	// Hold pluginsMu for the entire close so the moduledata-chain
	// unlink in pluginClose cannot interleave with a concurrent
	// Open's addmoduledata append (see pluginsMu's doc comment),
	// and so p.err / the path-cache entry change atomically with
	// respect to open()'s cached-entry check.
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	if p.err != "" {
		// Already closed (or failed to load); idempotent return.
		return nil
	}
	// Only the *Plugin Open handed out may be closed, and openedByOpen
	// is what decides it — the field test beside it only catches the
	// cheapest spelling of the misuse. Plugin is an exported struct, so
	// plugin.Plugin{} / new(plugin.Plugin) compile anywhere; such a
	// value has pluginpath == "" and loaded == nil. The host
	// executable's moduledata also has pluginpath == "" (the linker
	// only emits go:link.thispluginpath under -buildmode=plugin), so
	// handing "" to pluginClose would select the HOST module, mark it
	// bad and empty activeModules() — the next GC then has no roots and
	// no way to resolve PCs, and the process dies silently with exit
	// status 0 (trendvidia/go#35). The runtime refuses pluginpath == ""
	// independently; this check turns the misuse into an error before
	// the runtime is involved.
	//
	// A struct COPY of an opened Plugin (q := *p) passes both field
	// tests, so neither would stop it: closing the copy tears the
	// runtime state down and unmaps the image, then nils the COPY's
	// syms and sets the COPY's err, leaving Open's cached original with
	// err == "" and a syms map full of addresses the OS has unmapped.
	// unloadPlugin's cache cleanup misses it too (it deletes by
	// *Plugin identity), so the next Open of that path short-circuits
	// on the stale entry and hands those addresses back. Identity is
	// the only test that catches this.
	//
	// The refusal formats "" rather than p.pluginpath, and that is
	// not cosmetic: for a copy taken before the original's Close,
	// the copy still holds the pre-clone header into the plugin's
	// RODATA, which dlclose has since unmapped. pluginError.Error
	// concatenates the path, so naming it would fault inside the
	// very error this guard exists to hand back — on the path
	// Plugin.Close's doc tells callers to inspect. A value that did
	// not come from Open has no path worth naming anyway.
	if p.loaded == nil || p.pluginpath == "" || !openedByOpen(p) {
		return pluginErr("Close", "", "plugin was not opened by plugin.Open")
	}
	// p.pluginpath's bytes live in the plugin's RODATA — dlclose in
	// phase 2 unmaps that memory, leaving the string header dangling.
	// Anything that reads p.pluginpath after Close (e.g. lookup()'s
	// "symbol not found in <path>" error path) would then fault.
	// Clone into host memory before phase 1 so the field stays
	// usable for diagnostics, and so pluginErr below — which also
	// formats with p.pluginpath — works after a unloadPlugin failure.
	p.pluginpath = cloneString(p.pluginpath)
	if errstr := pluginClose(p.pluginpath, p.funcDescriptors); errstr != "" {
		return pluginErr("Close", p.pluginpath, errstr)
	}
	// Stop-the-world barrier between the moduledata unlink above and the
	// image unmap below: ensures no GC mark worker is still scanning the
	// plugin's data/bss roots (via markroot's activeModules() walk) when
	// unloadPlugin's dlclose/FreeLibrary frees those pages. Without it,
	// a worker holding the pre-unlink module slice faults on the unmapped
	// memory (a windows/amd64 Plugin.Close access violation in markroot).
	syncBeforeUnmap()
	if err := unloadPlugin(p); err != nil {
		// Runtime state is already torn down. Surface the dlclose
		// error but mark the plugin closed regardless: re-trying
		// Close would no-op (p.err is set below) and the OS
		// handle, if it survived dlclose's failure, is leaked
		// rather than retried.
		p.syms = nil
		p.funcDescriptors = nil
		p.err = "closed"
		return pluginErr("Close", p.pluginpath, err.Error())
	}
	p.syms = nil
	p.funcDescriptors = nil
	p.handle = 0
	p.err = "closed"
	return nil
}

// openedByOpen reports whether p is the *Plugin that Open cached for
// some path — the only kind of value Close may act on, and the only
// kind whose pluginpath bytes are known to still be mapped (see
// snapshotPath). Matching by
// pointer identity, not by path, is what distinguishes the cached
// original from a struct copy of it; unloadPlugin's cache cleanup
// scans the same map the same way. The map holds one entry per
// opened path, so the walk is over a handful of entries.
//
// Caller holds pluginsMu.
func openedByOpen(p *Plugin) bool {
	for _, q := range plugins {
		if q == p {
			return true
		}
	}
	return false
}

// pluginErr formats an error from a plugin operation. Kept simple
// and dependency-free so it works in stub builds too.
func pluginErr(op, path, msg string) error {
	return &pluginError{op: op, path: path, msg: msg}
}

// cloneString returns a copy of s whose backing bytes are
// freshly heap-allocated host memory, independent of any mapping s
// may have been spliced from. The plugin pkg uses this on Close to
// detach pluginpath from the plugin's RODATA before dlclose unmaps
// that page. Equivalent to strings.Clone, reinlined here so the
// plugin package keeps its small (errors-only) dependency surface.
func cloneString(s string) string {
	if len(s) == 0 {
		return ""
	}
	b := make([]byte, len(s))
	copy(b, s)
	return string(b)
}

type pluginError struct {
	op, path, msg string
}

func (e *pluginError) Error() string {
	return "plugin." + e.op + `("` + e.path + `"): ` + e.msg
}

// A Symbol is a pointer to a variable or function.
//
// For example, a plugin defined as
//
//	package main
//
//	import "fmt"
//
//	var V int
//
//	func F() { fmt.Printf("Hello, number %d\n", V) }
//
// may be loaded with the [Open] function and then the exported package
// symbols V and F can be accessed
//
//	p, err := plugin.Open("plugin_name.so")
//	if err != nil {
//		panic(err)
//	}
//	v, err := p.Lookup("V")
//	if err != nil {
//		panic(err)
//	}
//	f, err := p.Lookup("F")
//	if err != nil {
//		panic(err)
//	}
//	*v.(*int) = 7
//	f.(func())() // prints "Hello, number 7"
type Symbol any
