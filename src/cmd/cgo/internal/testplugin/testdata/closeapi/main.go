// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises the Plugin.Close, Plugin.HasDependents, and
// Plugin.Dependents API surface added by L3.b. Loads a single
// plugin, verifies the dependency-query state, closes it, and
// verifies subsequent Lookup returns an error.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	p, err := plugin.Open("closeapi-plugin1" + pluginExt)
	if err != nil {
		fail("Open(closeapi-plugin1): %v", err)
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

	fmt.Println("closeapi ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "closeapi: "+format+"\n", args...)
	os.Exit(1)
}
