// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "unsafe"

// A Layout describes heap objects by their size, alignment and which of
// their words hold pointers, and nothing else: no fields, names or
// methods. It is for a virtual machine that keeps guest values in
// ordinary Go memory and needs a Go type for each guest layout.
//
// Unlike a type made by reflect.StructOf, a Layout is not cached and is
// collected: once nothing refers to it and no object allocated from it
// is live, its memory is reclaimed. An object keeps its layout's type
// alive for as long as the object lives, so a Layout may be dropped
// while objects allocated from it remain in use.
//
// A Layout is safe for concurrent use.
type Layout struct {
	typ  unsafe.Pointer // the runtime's type; see runtime.goplus_newLayoutType
	size uintptr
}

// NewLayout returns a layout for objects of size bytes, aligned to
// align, whose pointer words are given by ptrmask: bit i%8 of
// ptrmask[i/8] set means that word i (the pointer-sized word at offset
// i*unsafe.Sizeof(uintptr(0))) holds a pointer the collector must trace.
// Bits past the end of the object are ignored, and a short ptrmask
// leaves the remaining words without pointers.
//
// size must be positive, a multiple of align, and no larger than the
// largest object the runtime can allocate. align must be a power of two
// no larger than the pointer size, and must equal it if any word holds a
// pointer. NewLayout panics otherwise.
//
// A word marked as a pointer must only ever hold nil or a valid pointer,
// as a Go pointer field must; the other words may hold anything. A store
// of a pointer into a pointer word must be recorded while the collector
// is marking, as Go code storing through a typed pointer does; generated
// code does so as [WriteBarrierFlag] describes.
func NewLayout(size, align uintptr, ptrmask []byte) *Layout {
	const ptrSize = unsafe.Sizeof(uintptr(0))
	if size == 0 {
		panic("goplus: NewLayout with size 0")
	}
	if align == 0 || align > ptrSize || align&(align-1) != 0 {
		panic("goplus: NewLayout alignment must be a power of two no larger than the pointer size")
	}
	if size%align != 0 {
		panic("goplus: NewLayout size is not a multiple of its alignment")
	}
	// ptrWords ends at the last pointer word.
	words := size / ptrSize
	ptrWords := uintptr(0)
	for i := uintptr(0); i < words && i/8 < uintptr(len(ptrmask)); i++ {
		if ptrmask[i/8]&(1<<(i%8)) != 0 {
			ptrWords = i + 1
		}
	}
	if ptrWords != 0 && align != ptrSize {
		panic("goplus: NewLayout with pointer words must be pointer-aligned")
	}
	typ := runtime_newLayoutType(size, align, ptrWords, ptrmask)
	if typ == nil {
		panic("goplus: NewLayout size out of range")
	}
	return &Layout{typ: typ, size: size}
}

// Size reports the size in bytes of one object of the layout.
func (l *Layout) Size() uintptr { return l.size }

// New allocates a zeroed object of the layout and returns a pointer to it.
func (l *Layout) New() unsafe.Pointer { return runtime_layoutNew(l.typ) }

// NewArray allocates n contiguous zeroed objects of the layout, l.Size()
// bytes apart, and returns a pointer to the first. It panics if n is
// negative or the total size overflows.
func (l *Layout) NewArray(n int) unsafe.Pointer { return runtime_layoutNewArray(l.typ, n) }

// Implemented in runtime.

//go:linkname runtime_newLayoutType
func runtime_newLayoutType(size, align, ptrWords uintptr, mask []byte) unsafe.Pointer

//go:linkname runtime_layoutNew
func runtime_layoutNew(t unsafe.Pointer) unsafe.Pointer

//go:linkname runtime_layoutNewArray
func runtime_layoutNewArray(t unsafe.Pointer, n int) unsafe.Pointer
