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
// A plugin is only initialized once, and cannot be closed.
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
// package — not merely a single toolchain/compiler version. The lock
// surface is every shared package: the runtime, google.golang.org/protobuf,
// and any other dependency the host and plugin both link. A standalone
// plugin build therefore loads only when the host resolves an identical
// dependency graph.
//
// The practical consequence: build the host and the plugin against the
// same resolved module graph. If the host resolves dependencies through
// a go.work overlay or a `replace` directive, the plugin must resolve
// them the same way; the reliable recipe is to build both with
// GOWORK=off against pinned go.mod versions (or both under the same
// workspace) so their per-package build hashes match.
//
// When a mismatch is detected, [Open] reports the offending package and
// both build hashes, for example:
//
//	plugin was built with a different version of package google.golang.org/protobuf/internal/pragma (plugin build hash 1a2b…, host build hash 9f8e…)
//
// so the diverging package — and which side is stale — is identifiable
// without decoding the raw loader message. See trendvidia/go#7.
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
	// panics with a recognisable message instead of jumping into
	// what dlclose has just unmapped.
	funcDescriptors []*uintptr
	// handle is the platform dlopen handle (or 0 in stub builds /
	// failed-load entries). Plugin.Close passes this to the
	// platform unloadPlugin path which calls dlclose, releasing
	// the OS-level mapping after the runtime side has rewritten
	// every held descriptor to a panic stub. Handles are not
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
// After a successful Close, [Plugin.Lookup] returns an error and
// function values previously obtained via Lookup must NOT be called
// — doing so is undefined behaviour. The application is responsible
// for releasing or never invoking such references across Close.
func (p *Plugin) Close() error {
	return closePlugin(p)
}

// HasDependents reports whether any other plugin currently holds
// canonicalised references into p. If false, [Plugin.Close] is
// guaranteed to succeed.
func (p *Plugin) HasDependents() bool {
	return pluginHasDependents(p.pluginpath)
}

// Dependents returns the pluginpaths of plugins that currently
// reference exported symbols from p. The application must Close
// those plugins (or release their references) before Closing p.
//
// The returned slice is a snapshot; concurrent loads or closes may
// invalidate it. The host program (the main executable) is omitted
// from the list since it does not Unload.
func (p *Plugin) Dependents() []string {
	return pluginDependents(p.pluginpath)
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
//  1. Runtime side (pluginClose): rewrites every closure descriptor
//     pointing into this plugin to runtime.pluginClosedFuncStub —
//     both the static descriptors emitted into the plugin's
//     .noptrdata (found via canonicalSyms) and the per-Lookup heap
//     holders threaded through funcDescriptors. Removes the
//     module's canonicalSyms entries and unlinks moduledata.
//
//  2. Platform side (unloadPlugin): dlclose / FreeLibraryEx. The OS
//     unmaps the plugin's text and data. Held func values are safe
//     to call: phase 1 already redirected them at the panic stub.
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
