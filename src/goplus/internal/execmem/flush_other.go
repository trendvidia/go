// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (darwin || linux) && !arm64

package execmem

// flushICache is a no-op where instruction fetch is coherent with data
// writes (amd64).
func flushICache(b []byte) {}
