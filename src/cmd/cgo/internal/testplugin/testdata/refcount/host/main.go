// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests the L3 cross-plugin dependency tracking end-to-end. This
// host deliberately does NOT import the mylib package that both
// plugins use, so plugA becomes the first-loader (canonical owner
// of mylib.Counter) and plugB's slot canonicalises to plugA's
// instance, creating the B→A dependency edge.
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
	a, err := plugin.Open("plugA" + pluginExt)
	if err != nil {
		fail("Open(plugA): %v", err)
	}
	b, err := plugin.Open("plugB" + pluginExt)
	if err != nil {
		fail("Open(plugB): %v", err)
	}

	incA, err := a.Lookup("IncA")
	if err != nil {
		fail("a.Lookup(IncA): %v", err)
	}
	incB, err := b.Lookup("IncB")
	if err != nil {
		fail("b.Lookup(IncB): %v", err)
	}

	// IncA bumps Counter by 1; IncB bumps by 2. Because both plugins'
	// mylib.Counter slots are canonicalised to plugA's instance, the
	// two functions modify the SAME Counter — totals are cumulative.
	if got := incA.(func() int)(); got != 1 {
		fail("first IncA() = %d, want 1 (canonicalisation broken: plugA's Counter not the canonical instance)", got)
	}
	if got := incB.(func() int)(); got != 3 {
		fail("first IncB() = %d, want 3 (canonicalisation broken: plugB writes its own Counter instead of plugA's)", got)
	}
	if got := incA.(func() int)(); got != 4 {
		fail("second IncA() = %d, want 4", got)
	}

	// plugB's mylib slots reference plugA, so plugA has plugB as a
	// dependent.
	if !a.HasDependents() {
		fail("a.HasDependents() = false; want true (plugB should depend on plugA via mylib.Counter)")
	}
	deps := a.Dependents()
	found := false
	for _, d := range deps {
		if d == "testplugin/refcount/plugB" {
			found = true
			break
		}
	}
	if !found {
		fail("a.Dependents() = %v; want list containing %q", deps, "testplugin/refcount/plugB")
	}

	// plugB shouldn't have any dependents.
	if b.HasDependents() {
		fail("b.HasDependents() = true; want false")
	}
	if got := b.Dependents(); len(got) != 0 {
		fail("b.Dependents() = %v; want empty", got)
	}

	// Close on plugA must fail while plugB still depends on it.
	if err := a.Close(); err == nil {
		fail("a.Close() returned nil while plugB still depends on plugA; want refused")
	}

	// Close plugB; plugA should now be closeable.
	if err := b.Close(); err != nil {
		fail("b.Close(): %v", err)
	}
	if a.HasDependents() {
		fail("after b.Close, a.HasDependents() = true; want false")
	}
	if err := a.Close(); err != nil {
		fail("a.Close() after b.Close: %v", err)
	}

	fmt.Println("refcount ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "refcount: "+format+"\n", args...)
	os.Exit(1)
}
