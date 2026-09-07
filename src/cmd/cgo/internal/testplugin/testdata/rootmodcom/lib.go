// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package rootcom sits at the root of module example.com, so its
// symbols are named `example%2ecom.X` — no slash, every dot escaped.
// It is imported by both rootmod plugins (plugA, plugB) but NOT by the
// host, so sharing Counter depends entirely on the runtime
// canonicalisation registry (userDataLink): there is no host export
// table entry to fall back on. See trendvidia/go#110.
package rootcom

// Counter is the shared global. First-loader-wins canonicalisation
// must make both plugins modify the FIRST plugin's instance.
var Counter int

// Bump adds n to Counter and returns the new value.
func Bump(n int) int {
	Counter += n
	return Counter
}
