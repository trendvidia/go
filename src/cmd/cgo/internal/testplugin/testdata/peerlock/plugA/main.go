// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// pm-plugA: first loader of testplugin/peerlock/peerlib (main-tree
// build). Loads before pm-plugB, so it owns peerlib's canonical
// instance; pm-plugB must be REJECTED by the version lock because its
// peerlib build hash differs (trendvidia/go#24).
package main

import "testplugin/peerlock/peerlib"

// Ver exposes the shared package so the host can observe which
// instance plugA holds.
func Ver() int { return peerlib.Len() }
