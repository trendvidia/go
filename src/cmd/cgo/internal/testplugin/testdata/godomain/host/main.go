// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests cross-module data canonicalisation for packages hosted under a
// go.*-style module domain (go.uber.org, go.opentelemetry.io, ...).
// isUserPackageName's old blanket `go.` prefix arm excluded every such
// package from userDataLink registration, so host and plugins each kept
// their own instance of the package's globals (trendvidia/go-archive#108).
//
// Two globals, two mechanisms:
//
//   - lib.Counter (plugins only, host does NOT import lib): sharing
//     rides on the runtime canonicalisation registry alone — plugA is
//     the first loader and owns the canonical instance, plugB's slots
//     must canonicalise to it.
//   - hostlib.Value (host AND plugA): on Windows sharing rides on the
//     host's export table / import library, which the peSkipSymName
//     slot-family filter must not swallow.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"

	"go.example.org/fakemod/hostlib"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	a, err := plugin.Open("godomain-plugA" + pluginExt)
	if err != nil {
		fail("Open(godomain-plugA): %v", err)
	}
	b, err := plugin.Open("godomain-plugB" + pluginExt)
	if err != nil {
		fail("Open(godomain-plugB): %v", err)
	}

	bumpA, err := a.Lookup("BumpA")
	if err != nil {
		fail("a.Lookup(BumpA): %v", err)
	}
	bumpB, err := b.Lookup("BumpB")
	if err != nil {
		fail("b.Lookup(BumpB): %v", err)
	}

	// BumpA bumps lib.Counter by 1; BumpB bumps by 2. Both plugins'
	// slots must canonicalise to plugA's instance, so the totals are
	// cumulative across plugins.
	if got := bumpA.(func() int)(); got != 1 {
		fail("first BumpA() = %d, want 1 (go.*-domain package excluded from canonicalisation: plugA's Counter not canonical)", got)
	}
	if got := bumpB.(func() int)(); got != 3 {
		fail("first BumpB() = %d, want 3 (go.*-domain package excluded from canonicalisation: plugB writes its own Counter)", got)
	}
	if got := bumpA.(func() int)(); got != 4 {
		fail("second BumpA() = %d, want 4", got)
	}

	// hostlib.Value is linked by the host too, so host and plugA must
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
	if hostlib.Value != 42 {
		fail("after plugA SetShared(42), host reads hostlib.Value = %d, want 42 (go.*-domain global not host-shared)", hostlib.Value)
	}
	hostlib.Value = 7
	if got := getShared.(func() int)(); got != 7 {
		fail("after host write, plugA GetShared() = %d, want 7 (go.*-domain global not host-shared)", got)
	}

	fmt.Println("godomain ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "godomain: "+format+"\n", args...)
	os.Exit(1)
}
