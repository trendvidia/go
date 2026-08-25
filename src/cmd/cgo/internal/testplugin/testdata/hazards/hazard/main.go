// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Hazard plugin for TestCloseHazardEnvelope: exports a variable
// whose storage lives in this plugin's data section, and a factory
// whose returned closure is created at runtime inside the plugin —
// its descriptor is registered neither in canonicalSyms nor in the
// per-Lookup holder slice, so Plugin.Close cannot rewrite it.
// Deliberately dependency-free: no imports means no shared packages
// to re-init when the host reopens this path.
package main

// Counter lives in the plugin's data section; a *int to it obtained
// via Lookup points into memory FreeLibrary/dlclose unmaps at Close.
var Counter int

// MakeBumper returns a closure allocated at runtime inside the
// plugin. Unlike Lookup results, nothing records its descriptor, so
// calling it after Close jumps into unmapped text.
func MakeBumper() func() int {
	return func() int {
		Counter++
		return Counter
	}
}
