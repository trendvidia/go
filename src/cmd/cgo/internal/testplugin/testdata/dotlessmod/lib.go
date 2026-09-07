// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package extensions sits at the root of a module path that has no dot
// to escape and no slash, so its symbols are spelled `extensions.X`.
// It is imported by both dotless plugins but NOT by the host, so
// sharing Counter depends entirely on the canonicalisation registry.
package extensions

// Counter is the shared global. First-loader-wins must make both
// plugins modify the FIRST plugin's instance.
var Counter int

// Bump adds n to Counter and returns the new value.
func Bump(n int) int {
	Counter += n
	return Counter
}
