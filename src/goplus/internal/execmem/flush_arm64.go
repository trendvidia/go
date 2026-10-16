// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package execmem

import "unsafe"

// flushICache makes code just written to b visible to instruction fetch:
// it cleans the data cache and invalidates the instruction cache for
// every line of b, then synchronises. Both operations are allowed at EL0
// on linux and darwin.
func flushICache(b []byte) {
	clearCache(uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
}

//go:noescape
func clearCache(addr, n uintptr)
