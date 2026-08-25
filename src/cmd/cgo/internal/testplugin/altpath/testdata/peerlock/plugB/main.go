// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// pm-plugB: built from the ALT tree, so its testplugin/peerlock/peerlib
// is the divergent build while pm-plugA (main tree) holds the original.
// The host loads A first, then must see B's Open FAIL with the standard
// named-package version error (trendvidia/go#24) — not a silent bind to
// A's instance. Built into the main tree as pm-plugB by TestMain; the
// import path is identical to plugA's peerlib on purpose.
package main

import "testplugin/peerlock/peerlib"

func Ver() int { return peerlib.Len() }
