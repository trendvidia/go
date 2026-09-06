// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package lib is imported by both godomain plugins (plugA, plugB) but
// NOT by the host, so sharing Counter depends entirely on the runtime
// canonicalisation registry (userDataLink) — there is no host export
// table entry to fall back on. Its import path lives under a
// go.*-style module domain: isUserPackageName's old blanket `go.`
// prefix arm excluded every such package from registration, so each
// plugin kept its own Counter (trendvidia/go#108).
package lib

// Counter is the shared global. First-loader-wins canonicalisation
// must make both plugins modify the FIRST plugin's instance.
var Counter int

// Bump adds n to Counter and returns the new value.
func Bump(n int) int {
	Counter += n
	return Counter
}
