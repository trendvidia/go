// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mylib is imported by both refcount plugins (plugA, plugB)
// but NOT by the host. Under -buildmode=plugin each plugin compiles
// its own copy of this package; at plugin-load time L3.a's
// canonicalisation rewrites the second-loader's slots to point at
// the first-loader's instance, creating a B→A dependency edge that
// L3.b's HasDependents/Dependents/Close API exercises.
package mylib

// Counter is the shared global whose canonicalisation creates the
// dependency edge. Both plugins read and write it via go.globalptr
// indirection; first-loader-wins means both plugins end up
// modifying the FIRST plugin's instance.
var Counter int

// Bump adds n to Counter and returns the new value.
func Bump(n int) int {
	Counter += n
	return Counter
}
