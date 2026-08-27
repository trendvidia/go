// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Closes Plugin values that plugin.Open never handed out
// (trendvidia/go#35). plugin.Plugin is an exported struct, so both
// spellings of the misuse compile anywhere:
//
//   - the zero value. Before the fix closePlugin passed
//     pluginpath == "" to the runtime, which matched the HOST module
//     (its moduledata has no pluginpath), marked it bad, and emptied
//     activeModules(): Close returned nil, and the next GC died
//     inside the collector with exit status 0 and no output.
//
//   - a struct copy of an opened Plugin (q := *p). Every field test
//     passes, so the copy closes for real — unmapping the image —
//     while Open's cached original keeps err == "" and a syms map of
//     addresses the OS has just unmapped, which the next Open of the
//     same path hands straight back.
//
// The fixture asserts Close reports an error for both, that the
// original survives its copy's rejection and still closes, that
// rejecting the copy AGAIN once the original has closed neither
// formats nor snapshots its now-dangling pluginpath, and —
// while no plugin has ever been unloaded — forces a GC, a stack
// capture and allocation churn, everything that needs the host
// module's roots and PC tables, printing "survived".
//
// Order matters: the survival check runs BEFORE the copy case opens
// a real plugin, so what it measures is the rejected Closes and
// nothing else. A GC after a plugin has genuinely been unloaded
// faults on windows/amd64 for an unrelated reason — the runtime's
// sync.Pool cleanup hook still points into the unmapped image
// (trendvidia/go#38) — and would make this fixture red for a defect
// it was not written to catch.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"runtime/debug"
	"strings"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	var byValue plugin.Plugin
	check("var p plugin.Plugin", byValue.Close())
	check("&plugin.Plugin{}", (&plugin.Plugin{}).Close())
	check("new(plugin.Plugin)", new(plugin.Plugin).Close())

	// A second Close on the same rejected value must keep
	// erroring: rejection must not have marked it "closed".
	check("second Close on zero value", byValue.Close())

	// Host-module accessors on a zero value must not fault or
	// match the host either.
	if byValue.HasDependents() {
		fail("zero-value HasDependents() = true, want false")
	}
	if deps := byValue.Dependents(); deps != nil {
		fail("zero-value Dependents() = %v, want nil", deps)
	}

	// A nil *Plugin is the documented no-op, not an error.
	var nilPlugin *plugin.Plugin
	if err := nilPlugin.Close(); err != nil {
		fail("(*plugin.Plugin)(nil).Close() = %v, want nil", err)
	}

	// Everything below dies silently if the host module was
	// unlinked. This runs before any real plugin is loaded, so it
	// answers one question only: did rejecting those values leave
	// the host's moduledata on the active list? A plugin that has
	// actually been unloaded brings a separate, unrelated hazard
	// into the same GC (trendvidia/go#38), which would otherwise
	// decide this fixture's result on some platforms.
	runtime.GC()
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, false)
	if !strings.Contains(string(buf[:n]), "main.main") {
		fail("runtime.Stack did not resolve main.main:\n%s", buf[:n])
	}
	var keep [][]byte
	for i := 0; i < 200000; i++ {
		keep = append(keep, make([]byte, 64))
		if i%50000 == 0 {
			runtime.GC()
		}
	}
	runtime.GC()
	fmt.Printf("survived (len %d)\n", len(keep))

	// The copy case needs a real plugin behind it. It comes last
	// because its final Close really unloads the image, and this
	// fixture must not GC after that (see trendvidia/go#38); stop
	// the collector so no background allocation can start one.
	debug.SetGCPercent(-1)
	p, err := plugin.Open("zeroclose-plugin1" + pluginExt)
	if err != nil {
		fail("Open(zeroclose-plugin1): %v", err)
	}
	byCopy := *p
	check("copy of an Open-returned Plugin", byCopy.Close())

	// Rejecting the copy must have left the original untouched:
	// still looked-up-able, and still closable exactly once.
	f, err := p.Lookup("F")
	if err != nil {
		fail("Lookup(F) after the copy was rejected: %v", err)
	}
	if got := f.(func() int)(); got != 3 {
		fail("F() after the copy was rejected = %d, want 3", got)
	}
	if err := p.Close(); err != nil {
		fail("Close() of the original: %v", err)
	}

	// The copy now outlives the image: its pluginpath still holds
	// the pre-clone header into the plugin's RODATA, which the
	// original's Close unmapped (closePlugin clones only the
	// original's field). Rejecting it a second time must not read
	// those bytes — neither when formatting the error (Close's doc
	// tells callers to inspect it) nor when the accessors snapshot
	// the path.
	check("copy of a closed Plugin", byCopy.Close())
	if byCopy.HasDependents() {
		fail("stale copy HasDependents() = true, want false")
	}
	if deps := byCopy.Dependents(); deps != nil {
		fail("stale copy Dependents() = %v, want nil", deps)
	}
	fmt.Println("copy rejected, original closed")
}

func check(what string, err error) {
	if err == nil {
		fail("%s: Close() = nil, want error", what)
	}
	if !strings.Contains(err.Error(), "not opened by plugin.Open") {
		fail("%s: Close() = %q, want a \"not opened by plugin.Open\" error", what, err)
	}
	fmt.Printf("%s: %v\n", what, err)
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "zeroclose: "+format+"\n", args...)
	os.Exit(1)
}
