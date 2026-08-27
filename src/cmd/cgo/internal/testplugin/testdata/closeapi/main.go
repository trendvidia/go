// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises the Plugin.Close, Plugin.HasDependents, and
// Plugin.Dependents API surface added by L3.b. Loads a single
// plugin, verifies the dependency-query state, closes it, and
// verifies subsequent Lookup returns an error. Then re-opens the
// same path and verifies the plugin's init functions ran AGAIN —
// plugin1's init multiplies the host-shared common.X by 7, so the
// once-per-successful-Open initialization the package doc promises
// shows up as 3 → 21 → 147.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"

	"testplugin/common"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	if common.X != 3 {
		fail("common.X = %d before any Open; want 3 (host init only)", common.X)
	}

	p, err := plugin.Open("closeapi-plugin1" + pluginExt)
	if err != nil {
		fail("Open(closeapi-plugin1): %v", err)
	}

	// plugin1's init ran: common.X *= 7.
	if common.X != 21 {
		fail("common.X = %d after first Open; want 21 (plugin init ran once)", common.X)
	}

	// No other plugin loaded → no dependents.
	if p.HasDependents() {
		fail("HasDependents() = true on freshly-loaded single plugin; want false")
	}
	if got := p.Dependents(); len(got) != 0 {
		fail("Dependents() = %v on freshly-loaded single plugin; want empty", got)
	}

	// Look up a symbol so the slot machinery is exercised.
	if _, err := p.Lookup("F"); err != nil {
		fail("Lookup(F) before Close: %v", err)
	}

	// Close should succeed since nothing depends on the plugin.
	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}

	// After Close, Lookup must fail.
	if _, err := p.Lookup("F"); err == nil {
		fail("Lookup(F) after Close returned no error; want error")
	}

	// Idempotent: a second Close is a no-op.
	if err := p.Close(); err != nil {
		fail("second Close(): %v", err)
	}

	// Re-opening a closed path is a fresh load: unloadPlugin removed
	// the plugins[path] entry, so this Open dlopens anew and re-runs
	// the plugin's init functions — "initialized once" holds per
	// successful Open, not per path for the life of the process.
	p2, err := plugin.Open("closeapi-plugin1" + pluginExt)
	if err != nil {
		fail("reopen Open(closeapi-plugin1): %v", err)
	}
	if common.X != 147 {
		fail("common.X = %d after reopen; want 147 (plugin init ran again)", common.X)
	}
	if fn, err := p2.Lookup("F"); err != nil {
		fail("Lookup(F) on reopened plugin: %v", err)
	} else if got := fn.(func() int)(); got != 3 {
		fail("F() on reopened plugin = %d, want 3", got)
	}
	if err := p2.Close(); err != nil {
		fail("Close() of reopened plugin: %v", err)
	}

	fmt.Println("closeapi ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "closeapi: "+format+"\n", args...)
	os.Exit(1)
}
