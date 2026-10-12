// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package shared is linked into both the canonicaldata host and its
// plugin, so each module carries its own copy of every descriptor and
// global below. The fixture's checks all reduce to one question: does
// a value produced by the plugin behave as if built from the host's
// (canonical) copy?
package shared

// Concrete is the type both modules box, assert, and compare.
type Concrete struct{ V int }

func (Concrete) M() {}

// I is the non-empty interface both modules convert Concrete into.
type I interface{ M() }

// MyErr backs the statically-initialized error global in the plugin.
type MyErr struct{ Code int }

func (MyErr) Error() string { return "myerr" }

// Global and Arr are the targets of the plugin's statically-embedded
// addresses (trendvidia/go-archive#25, edge 4). Both modules must observe one
// instance of each.
var Global int

var Arr [4]int

// Fn is the function the plugin materializes a func VALUE for. The
// host calls it too, so it is exported from the host image and the
// plugin's own copy of the body is replaced by the host-shared shape
// (SWINDOWS under an internal link, SHOSTOBJ under an external one) —
// which is what puts a TEXT symbol on the receiving end of the
// `Fn·f` funcval container's R_ADDR.
func Fn() int { return 11 }
