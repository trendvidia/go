// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// victim is the plugin the rejectchained host exercises: built twice
// into sibling directories, one copy byte-patched by the host to carry
// a foreign-generation ABI marker and loaded raw, the other opened
// normally afterwards as the healthy control. Identical sources on
// purpose — the pair must share pkghashes, so any effect the resident
// rejected image has on the second load is attributable to the
// rejection residue, not to divergent builds.
package main

// Name identifies this plugin.
func Name() string { return "V" }
