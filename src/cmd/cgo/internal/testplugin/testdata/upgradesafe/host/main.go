// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Demonstrates and pins the upgrade contract Plugin.Close documents: a
// host that reaches a plugin only through Lookup-obtained function
// values, and exchanges only types it already knows, can replace one
// build with another with nothing unsafe to release first.
//
// This is the counterpart of the upgrade fixture, which pins what
// happens when the host breaks the contract by caching a pointer into
// the plugin's data. Both matter: that one proves the failure is loud,
// this one proves the supported path is clean — a fix that made every
// upgrade fault would satisfy the first test and be useless.
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
	v1, err := plugin.Open("upgradesafe-v1" + pluginExt)
	if err != nil {
		fail("Open(v1): %v", err)
	}
	scoreV1 := lookupScore(v1, "v1")
	if got := scoreV1("abcd"); got != 4 {
		fail("v1 Score(abcd) = %d, want 4", got)
	}
	if got := lookupVersion(v1, "v1")(); got != 1 {
		fail("v1 Version() = %d, want 1", got)
	}

	// Nothing held here points into v1: scoreV1 is a Lookup func
	// value, and the string and int it traffics in are the host's.
	if err := v1.Close(); err != nil {
		fail("Close(v1): %v", err)
	}

	v2, err := plugin.Open("upgradesafe-v2" + pluginExt)
	if err != nil {
		fail("Open(v2): %v", err)
	}
	scoreV2 := lookupScore(v2, "v2")
	if got := scoreV2("abcd"); got != 40 {
		fail("v2 Score(abcd) = %d, want 40 (the upgraded build must answer)", got)
	}
	if got := lookupVersion(v2, "v2")(); got != 2 {
		fail("v2 Version() = %d, want 2", got)
	}

	// The leftover handle on the previous build must fail the way the
	// documentation promises: a recoverable panic, not a fault, and
	// not a silent call into the new image either.
	func() {
		defer func() {
			r := recover()
			if r == nil {
				fail("calling v1's Score after Close returned instead of panicking")
			}
			msg, ok := r.(string)
			if !ok {
				fail("recovered %T from the stale call, want the documented string", r)
			}
			if msg != "plugin: function value invoked after Plugin.Close" {
				fail("recovered %q, want the documented panic value", msg)
			}
		}()
		scoreV1("abcd")
	}()

	// ...and the upgraded plugin is unharmed by that panic.
	if got := scoreV2("abcdef"); got != 60 {
		fail("v2 Score after the stale call = %d, want 60", got)
	}

	fmt.Println("upgradesafe ok")
}

func lookupScore(p *plugin.Plugin, which string) func(string) int {
	sym, err := p.Lookup("Score")
	if err != nil {
		fail("%s.Lookup(Score): %v", which, err)
	}
	f, ok := sym.(func(string) int)
	if !ok {
		fail("%s Score is %T, want func(string) int", which, sym)
	}
	return f
}

func lookupVersion(p *plugin.Plugin, which string) func() int {
	sym, err := p.Lookup("Version")
	if err != nil {
		fail("%s.Lookup(Version): %v", which, err)
	}
	return sym.(func() int)
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "upgradesafe: "+format+"\n", args...)
	os.Exit(1)
}
