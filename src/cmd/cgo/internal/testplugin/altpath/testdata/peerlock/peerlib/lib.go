// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Divergent build of testplugin/peerlock/peerlib for pm-plugB
// (trendvidia/go#24): same import path as the main-tree copy that
// pm-plugA loads first, different source — hence a different
// linktimehash. The version lock must reject pm-plugB instead of
// letting its accesses silently bind to pm-plugA's instance. The
// content difference itself is arbitrary; any change alters the hash.
package peerlib

var Tag = "plugB-build-with-a-different-layout"

func Len() int { return len(Tag) }
