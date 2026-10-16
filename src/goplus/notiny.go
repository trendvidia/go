// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import _ "unsafe" // for go:linkname

// SetNoTinyAlloc sets whether allocations made by the calling goroutine
// bypass the tiny allocator, and reports the previous setting. Goroutines
// it starts afterwards inherit the setting.
//
// The tiny allocator packs pointer-free allocations under 16 bytes,
// from any goroutine on the same P, into shared 16-byte blocks, so
// [ObjectExtent] of one of them spans its neighbours too. With the
// setting on, each such allocation gets a 16-byte slot of its own. That
// holds for every allocation the goroutine causes, including those made
// for it inside the runtime and the standard library: string
// concatenation, conversions, append growth.
//
// The cost is memory: a 1-byte allocation occupies 16 bytes. Goroutines
// without the setting are unaffected.
func SetNoTinyAlloc(on bool) (prev bool) {
	return runtime_setNoTinyAlloc(on)
}

//go:linkname runtime_setNoTinyAlloc
func runtime_setNoTinyAlloc(on bool) (prev bool)
