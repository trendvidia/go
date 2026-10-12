// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Host for TestCloseHazardEnvelope (trendvidia/go-archive#26). Each
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
//     When the loader maps the new instance at the base the closed
//     one occupied — which it usually, but not always, does — the
//     stale pointer observes writes made through the fresh handle,
//     and the stale closure runs new-instance code against the new
//     Counter. This is the documented silent-aliasing hazard of
//     reusing a stale reference past Close. The probe first
//     requires the reopened Counter to read 0 — its declared
//     initial value, which a reopen that merely handed back the
//     still-live closed instance would not show, since the
//     pre-Close call left that one at 1. It then checks the
//     reopened base BEFORE touching anything stale: if the base
//     moved, the stale references point at unmapped memory (a fatal
//     fault) or at whatever now occupies that range (silent
//     corruption), neither of which this probe can assert safely, so
//     it reports both bases and skips (trendvidia/go-archive#47).
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

	// The reopened instance must be a NEW instance, observed at its
	// declared initial state: Counter is `var Counter int`, and the
	// pre-Close staleF() above left the CLOSED instance's Counter at
	// 1. So a 0 here is the fact that everything below rests on. If
	// Close ever stopped dropping the plugins[] cache entry, or the
	// loader handed back the still-resident original instead of
	// reloading the image, this reads 1 and the whole aliasing cell
	// would otherwise pass while comparing an instance with itself.
	// Checked before the base comparison so it runs on both paths.
	if got := *freshCtr; got != 0 {
		fail("reopened Counter = %d; want 0 (a reopen must produce a fresh instance, not the closed one)", got)
	}

	// Counter sits at the same image-relative offset in both
	// instances, so the two Lookup results are equal exactly when the
	// loader mapped the reopened image at the closed one's base.
	//
	// That USED to be the common outcome, and this cell existed to
	// document it: the loader reused a freed base despite ASLR, so a
	// stale pointer silently addressed the new instance. Unload now
	// reserves the range the image occupied (trendvidia/go-archive#122 on
	// Windows, #123 on POSIX), so the reopen cannot land there and the
	// aliasing is gone by construction.
	//
	// The assertion is therefore inverted from what it was. Equal
	// addresses no longer mean "the loader happened to reuse the
	// base"; they mean the reservation did not happen, and the silent
	// corruption it prevents is back. The upgrade fixture covers the
	// consequence — a stale write landing in the NEXT plugin — while
	// this one pins the mechanism at its source.
	if freshCtr == staleCtr {
		fail("reopened at the SAME base (Counter %p): the freed range was not reserved, so a "+
			"pointer held across Close silently addresses the new instance", freshCtr)
	}
	fmt.Printf("hazards alias ok: reopened at a different base (stale Counter %p, fresh Counter %p)\n", staleCtr, freshCtr)
	_ = staleF
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "hazards: "+format+"\n", args...)
	os.Exit(1)
}
