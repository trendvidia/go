// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The runtime half of package goplus, the go-plus fork's API for a VM
// whose guest values are ordinary Go memory.

package runtime

import (
	"internal/runtime/gc"
	_ "unsafe" // for go:linkname
)

// goplus_objectExtent returns the base address and slot size of the heap
// object containing p, or base == 0 if p does not point into one.
//
// Unlike findObject it never reports a bad pointer: the caller is asking
// about a pointer it does not trust to be in the heap.
//
// It is nosplit because p may point into the calling goroutine's stack,
// and as a uintptr it would not be adjusted if the stack moved.
//
//go:linkname goplus_objectExtent goplus.runtime_objectExtent
//go:nosplit
func goplus_objectExtent(p uintptr) (base, size uintptr) {
	s := spanOfHeap(p)
	if s == nil {
		return 0, 0
	}
	idx := s.objIndex(p)
	base = s.base() + idx*s.elemsize
	size = s.elemsize
	// A small object whose pointer map does not fit in the span's heap
	// bits starts with a malloc header holding its type. The allocator
	// returns the address after it (see typePointersOfUnchecked).
	if !s.spanclass.noscan() && !heapBitsInSpan(s.elemsize) && s.spanclass.sizeclass() != 0 {
		base += gc.MallocHeaderSize
		size -= gc.MallocHeaderSize
		if p < base {
			// p points at the header, which no Go value occupies.
			return 0, 0
		}
	}
	return base, size
}

// goplus_setNoTinyAlloc sets whether the calling goroutine bypasses the
// tiny allocator (see mallocgc and the tiny malloc stubs), and returns
// the previous setting. newproc1 copies it to new goroutines.
//
//go:linkname goplus_setNoTinyAlloc goplus.runtime_setNoTinyAlloc
func goplus_setNoTinyAlloc(on bool) (prev bool) {
	gp := getg()
	prev = gp.goplusNoTiny
	gp.goplusNoTiny = on
	return prev
}
