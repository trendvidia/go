// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Host for TestCloseHazardEnvelope (trendvidia/go#26). Each
// subcommand loads hazards-plugin, takes one reference class into
// the plugin, closes it, then uses the stale reference:
//
//   - closure: a runtime-created closure from MakeBumper. Calling it
//     after Close must kill the process with "fatal error: fault" —
//     the stub rewrite cannot reach its descriptor.
//
//   - dataptr: a *int to the exported Counter. Writing through it
//     after Close must likewise fault.
//
//   - alias: same references, but the path is REOPENED before use.
//     The loader maps the new instance at the reused addresses, so
//     the stale pointer must observe writes made through the fresh
//     handle, and the stale closure must run new-instance code
//     against the new Counter. This is the documented silent-
//     aliasing hazard of reusing a stale reference past Close.
//
// The fatal probes never return on success; the parent test requires
// a nonzero exit whose output mentions "fatal error: fault". If a
// probe ever SURVIVES (e.g. a stub rewrite starts over-reaching and
// catches these classes too), it prints so and exits cleanly — the
// parent fails either way, with the reason in the child's stdout.

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

const pluginName = "hazards-plugin"

func main() {
	if len(os.Args) != 2 {
		fail("usage: hazards <closure|dataptr|alias>")
	}
	switch os.Args[1] {
	case "closure":
		closureProbe()
	case "dataptr":
		dataptrProbe()
	case "alias":
		aliasProbe()
	default:
		fail("unknown probe %q", os.Args[1])
	}
}

func open() *plugin.Plugin {
	p, err := plugin.Open(pluginName + pluginExt)
	if err != nil {
		fail("Open(%s): %v", pluginName+pluginExt, err)
	}
	return p
}

// lookupBumper calls into the plugin to obtain a runtime-created
// closure — the unprotected function-reference class.
func lookupBumper(p *plugin.Plugin) func() int {
	sym, err := p.Lookup("MakeBumper")
	if err != nil {
		fail("Lookup(MakeBumper): %v", err)
	}
	mk, ok := sym.(func() func() int)
	if !ok {
		fail("MakeBumper is %T; want func() func() int", sym)
	}
	return mk()
}

// lookupCounter takes a pointer to the plugin's exported variable —
// the unprotected data-reference class.
func lookupCounter(p *plugin.Plugin) *int {
	sym, err := p.Lookup("Counter")
	if err != nil {
		fail("Lookup(Counter): %v", err)
	}
	ptr, ok := sym.(*int)
	if !ok {
		fail("Counter is %T; want *int", sym)
	}
	return ptr
}

func closureProbe() {
	p := open()
	f := lookupBumper(p)
	if got := f(); got != 1 {
		fail("pre-Close f() = %d; want 1", got)
	}
	if err := p.Close(); err != nil {
		fail("Close: %v", err)
	}
	got := f() // must fault; never returns
	fmt.Println("SURVIVED: post-Close closure call returned", got)
}

func dataptrProbe() {
	p := open()
	ctr := lookupCounter(p)
	*ctr = 7
	if err := p.Close(); err != nil {
		fail("Close: %v", err)
	}
	*ctr = 8 // must fault; never returns
	fmt.Println("SURVIVED: post-Close write through stale *Counter")
}

func aliasProbe() {
	p := open()
	staleCtr := lookupCounter(p)
	staleF := lookupBumper(p)
	if got := staleF(); got != 1 {
		fail("pre-Close staleF() = %d; want 1", got)
	}
	if err := p.Close(); err != nil {
		fail("Close: %v", err)
	}

	fresh := open()
	freshCtr := lookupCounter(fresh)
	*freshCtr = 100

	// The stale pointer aliases the reopened instance's storage...
	if got := *staleCtr; got != 100 {
		fail("*staleCtr after reopen = %d; want 100 (aliased)", got)
	}
	// ...and the stale closure runs the new instance's code.
	if got := staleF(); got != 101 {
		fail("staleF() after reopen = %d; want 101 (new instance)", got)
	}
	if got := *freshCtr; got != 101 {
		fail("*freshCtr after staleF() = %d; want 101", got)
	}
	fmt.Println("hazards alias ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "hazards: "+format+"\n", args...)
	os.Exit(1)
}
