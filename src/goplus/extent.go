// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "unsafe"

// ObjectExtent reports the heap object containing p: the address the
// allocator returned for it, and the size of the slot it occupies.
//
// ok is false when p does not point into an object in the Go heap: a
// pointer into a goroutine stack, a global variable, memory allocated
// outside Go (C, mmap), or nil. The caller treats those as extent
// unknown.
//
// The size is the allocator's slot, not the size that was requested. It
// is the requested size rounded up to the object's size class, or to a
// whole number of pages for a large object, so it may include padding
// past the end of the value. Small pointer-free objects may also be
// packed several to a slot by the tiny allocator; for those the extent
// covers the whole 16-byte block, neighbours included. A caller that
// needs the exact bounds of a value, rather than of its allocation,
// combines the extent with the value's type.
//
// ObjectExtent is safe to call concurrently with garbage collection. As
// long as the caller holds p, the object stays allocated and the result
// stays valid.
func ObjectExtent(p unsafe.Pointer) (base unsafe.Pointer, size uintptr, ok bool) {
	// The runtime takes p as an integer: a pointer argument to a function
	// without a body would be assumed to escape, moving every value it is
	// asked about to the heap. p stays live across the call because it is
	// used below, and the runtime half is nosplit, so a stack cannot move
	// under it.
	b, n := runtime_objectExtent(uintptr(p))
	if b == 0 {
		return nil, 0, false
	}
	// base is derived from p, which points into the same allocation.
	return unsafe.Add(p, -(uintptr(p) - b)), n, true
}

// Implemented in runtime.

//go:linkname runtime_objectExtent
func runtime_objectExtent(p uintptr) (base, size uintptr)
