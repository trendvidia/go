// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The runtime half of package goplus, the go-plus fork's API for a VM
// whose guest values are ordinary Go memory.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/gc"
	"unsafe"
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

// goplus_writeBarrierFlag returns the address of the word compiled code
// tests before a heap pointer store (see writeBarrier).
//
//go:linkname goplus_writeBarrierFlag goplus.runtime_writeBarrierFlag
func goplus_writeBarrierFlag() *uint32 {
	return (*uint32)(unsafe.Pointer(&writeBarrier))
}

// goplus_writeBarrierEntry returns the PC of gcWriteBarrier<n>, the entry
// compiled code calls to reserve n slots in the write-barrier buffer, or
// 0 if n is out of range. Its register contract is documented on
// gcWriteBarrier in asm_*.s.
//
//go:linkname goplus_writeBarrierEntry goplus.runtime_writeBarrierEntry
func goplus_writeBarrierEntry(n int) uintptr {
	switch n {
	case 1:
		return abi.FuncPCABIInternal(gcWriteBarrier1)
	case 2:
		return abi.FuncPCABIInternal(gcWriteBarrier2)
	case 3:
		return abi.FuncPCABIInternal(gcWriteBarrier3)
	case 4:
		return abi.FuncPCABIInternal(gcWriteBarrier4)
	case 5:
		return abi.FuncPCABIInternal(gcWriteBarrier5)
	case 6:
		return abi.FuncPCABIInternal(gcWriteBarrier6)
	case 7:
		return abi.FuncPCABIInternal(gcWriteBarrier7)
	case 8:
		return abi.FuncPCABIInternal(gcWriteBarrier8)
	}
	return 0
}

// goplus.StackGuardOffset promises generated code that g.stackguard0 is
// at 2*PtrSize, the offset the assemblers already hardcode in every
// function prologue. This fails to compile if it moves.
const (
	_ = uintptr(unsafe.Offsetof(g{}.stackguard0) - 2*goarch.PtrSize)
	_ = uintptr(2*goarch.PtrSize - unsafe.Offsetof(g{}.stackguard0))
)
