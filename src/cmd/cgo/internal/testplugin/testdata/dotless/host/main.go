// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests cross-module canonicalisation for symbol names with NO SLASH
// (trendvidia/go-archive#115). isUserPackageName used to reject those, reading
// a missing slash as "stdlib top-level package". Nothing can tell the
// two apart from the name — `cmd/link` carries no std-package list and
// sym.Library has no GOROOT marker — so that heuristic cost correctness
// in both directions, and this fixture covers both:
//
//   - extensions.Counter: a package at the root of a module named
//     without a domain, which is what `go mod init extensions` gives
//     first-party extension code. Imported by both plugins, not the
//     host, so sharing rides on the canonicalisation registry alone.
//
//   - flag.CommandLine: a slashless STDLIB package that the plugins
//     link and the host does NOT, so the host's import library cannot
//     share it either. Two extensions importing the same stdlib
//     package each kept their own copy of its globals.
//
// POSIX gives one instance in both cases; these assertions are how
// Windows is held to that.
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
	a, err := plugin.Open("dotless-plugA" + pluginExt)
	if err != nil {
		fail("Open(dotless-plugA): %v", err)
	}
	b, err := plugin.Open("dotless-plugB" + pluginExt)
	if err != nil {
		fail("Open(dotless-plugB): %v", err)
	}

	bumpA := lookupInt(a, "BumpA")
	bumpB := lookupInt(b, "BumpB")

	// One Counter: BumpA adds 1, BumpB adds 2, so the totals are
	// cumulative across the two plugins.
	if got := bumpA(); got != 1 {
		fail("first BumpA() = %d, want 1 (root package of a dotless module path excluded from canonicalisation)", got)
	}
	if got := bumpB(); got != 3 {
		fail("first BumpB() = %d, want 3 (plugB writes its own Counter)", got)
	}
	if got := bumpA(); got != 4 {
		fail("second BumpA() = %d, want 4", got)
	}

	// Both plugins must see ONE flag.CommandLine. Read only after both
	// are loaded: flag's init assigns the variable, so if it is shared
	// the last loader's value is what everyone sees, and reading
	// between loads would differ even when sharing works.
	pa, pb := lookupPtr(a, "FlagsPtr")(), lookupPtr(b, "FlagsPtr")()
	if pa != pb {
		fail("plugA sees flag.CommandLine=%#x, plugB sees %#x: two plugins linking the same slashless "+
			"stdlib package kept separate copies of its globals", pa, pb)
	}

	fmt.Println("dotless ok")
}

func lookupInt(p *plugin.Plugin, name string) func() int {
	f, err := p.Lookup(name)
	if err != nil {
		fail("Lookup(%s): %v", name, err)
	}
	return f.(func() int)
}

func lookupPtr(p *plugin.Plugin, name string) func() uintptr {
	f, err := p.Lookup(name)
	if err != nil {
		fail("Lookup(%s): %v", name, err)
	}
	return f.(func() uintptr)
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "dotless: "+format+"\n", args...)
	os.Exit(1)
}
