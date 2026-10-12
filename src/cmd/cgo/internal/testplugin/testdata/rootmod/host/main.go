// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests cross-module data canonicalisation for a package sitting at the
// ROOT of a dotted module path (trendvidia/go-archive#110). objabi.PathToPrefix
// escapes the dots after the final slash, so a root package's symbols
// carry no slash at all — `module example.com` gives
// `example%2ecom.Counter` — and isUserPackageName's "no slash means
// stdlib" arm dropped them from userDataLink registration, leaving host
// and plugins with separate instances of the global on Windows. Same
// failure class as the godomain fixture (#108), different arm.
//
// Two globals, two mechanisms, mirroring godomain:
//
//   - rootcom.Counter (plugins only, host does NOT import it): sharing
//     rides on the runtime canonicalisation registry alone — plugA is
//     the first loader and owns the canonical instance, plugB's slots
//     must canonicalise to it.
//   - rootorg.Value (host AND plugA): on Windows sharing rides on the
//     host's export table / import library, which the peSkipSymName
//     filters must not swallow either.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"

	rootorg "example.org"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	a, err := plugin.Open("rootmod-plugA" + pluginExt)
	if err != nil {
		fail("Open(rootmod-plugA): %v", err)
	}
	b, err := plugin.Open("rootmod-plugB" + pluginExt)
	if err != nil {
		fail("Open(rootmod-plugB): %v", err)
	}

	bumpA, err := a.Lookup("BumpA")
	if err != nil {
		fail("a.Lookup(BumpA): %v", err)
	}
	bumpB, err := b.Lookup("BumpB")
	if err != nil {
		fail("b.Lookup(BumpB): %v", err)
	}

	// BumpA bumps rootcom.Counter by 1; BumpB bumps by 2. Both plugins'
	// slots must canonicalise to plugA's instance, so the totals are
	// cumulative across plugins.
	if got := bumpA.(func() int)(); got != 1 {
		fail("first BumpA() = %d, want 1 (root package of a dotted module path excluded from canonicalisation: plugA's Counter not canonical)", got)
	}
	if got := bumpB.(func() int)(); got != 3 {
		fail("first BumpB() = %d, want 3 (root package of a dotted module path excluded from canonicalisation: plugB writes its own Counter)", got)
	}
	if got := bumpA.(func() int)(); got != 4 {
		fail("second BumpA() = %d, want 4", got)
	}

	// rootorg.Value is linked by the host too, so host and plugA must
	// observe ONE instance: a plugin write is a host read and vice
	// versa.
	setShared, err := a.Lookup("SetShared")
	if err != nil {
		fail("a.Lookup(SetShared): %v", err)
	}
	getShared, err := a.Lookup("GetShared")
	if err != nil {
		fail("a.Lookup(GetShared): %v", err)
	}
	setShared.(func(int))(42)
	if rootorg.Value != 42 {
		fail("after plugA SetShared(42), host reads rootorg.Value = %d, want 42 (root-package global not host-shared)", rootorg.Value)
	}
	rootorg.Value = 7
	if got := getShared.(func() int)(); got != 7 {
		fail("after host write, plugA GetShared() = %d, want 7 (root-package global not host-shared)", got)
	}

	fmt.Println("rootmod ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "rootmod: "+format+"\n", args...)
	os.Exit(1)
}
