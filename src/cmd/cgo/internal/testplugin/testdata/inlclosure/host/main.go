// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests that two plugins' inlined copies of one closure keep their own
// bodies (trendvidia/go-archive#158).
//
// Each plugin inlines lib.Desc with a different constant, so each holds
// a copy of Desc's closure specialised on that constant, and both copies
// are named lib.Desc.func1. A go.funcptr slot named after the closure
// was canonicalised by that name at load, so the second plugin's slot
// was rewritten to the first plugin's copy and Get returned "A" twice.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
)

func get(name string) string {
	ext := ".so"
	if runtime.GOOS == "windows" {
		ext = ".dll"
	}
	p, err := plugin.Open(name + ext)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	g, err := p.Lookup("Get")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return g.(func() string)()
}

func main() {
	a, b := get("inlclosure-plugA"), get("inlclosure-plugB")
	if a != "A" || b != "B" {
		fmt.Fprintf(os.Stderr, "plugA.Get() = %q, plugB.Get() = %q; want \"A\", \"B\"\n", a, b)
		os.Exit(1)
	}
}
