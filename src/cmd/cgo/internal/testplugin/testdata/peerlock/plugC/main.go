// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// pm-plugC: same-tree build importing the SAME peerlib build as
// pm-plugA. Positive control: identical hashes must load cleanly and
// share A's instance (the version lock rejects only DIVERGENT builds).
package main

import "testplugin/peerlock/peerlib"

func Ver() int { return peerlib.Len() }
