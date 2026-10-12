// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package peerlib is the shared dependency that neither the peerlock
// host nor any validator links: it exists only inside pm-plugA/pm-plugC
// (this copy) and pm-plugB (the divergent copy in altpath/testdata).
// Its whole purpose is trendvidia/go-archive#24: two plugins linking different
// BUILD HASHES of the same import path that the host never sees. Any
// source difference changes the linktimehash, so the exact content is
// unimportant — but keep it dependency-free so nothing else enters the
// comparison.
package peerlib

// Tag distinguishes the builds: the main-tree copy is "plugA-build",
// the altpath copy is "plugB-build".
var Tag = "plugA-build"

// Len reports len(Tag) so hosts can observe the instance without
// exporting the variable directly.
func Len() int { return len(Tag) }
