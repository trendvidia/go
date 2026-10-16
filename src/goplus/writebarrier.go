// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import _ "unsafe" // for go:linkname

// WriteBarrierFlag returns the address of Go's write-barrier flag.
//
// While the collector is marking, every store of a pointer into the heap
// must be recorded, or the collector can free an object that is still
// reachable. Compiled Go code tests this flag before each such store and
// records the store only while it is set. Generated code does the same:
// load the 32-bit word at this address and, if it is nonzero, record the
// store through [WriteBarrierEntry] before making it. Stores of scalars,
// and of pointers into memory outside the Go heap, need nothing.
func WriteBarrierFlag() *uint32 {
	return runtime_writeBarrierFlag()
}

// WriteBarrierEntry returns the address of the runtime entry that
// reserves n pointer-sized slots, 1 <= n <= 8, in the current P's
// write-barrier buffer. It panics if n is out of range.
//
// Recording a store of pointer v to *p takes two slots: v, then the old
// value of *p, as compiled code does:
//
//	if *WriteBarrierFlag() != 0 {
//		buf := call entry(2)
//		buf[0] = v
//		buf[1] = *p
//	}
//	*p = v
//
// Several stores can share one call with a larger n.
//
// The entry does not follow a Go calling convention. Its contract:
//
//   - amd64: CALL the address. On return R11 points to the n slots.
//     Every other general-purpose register is preserved; the flags are
//     clobbered, and so may be the vector registers. R14 must hold the
//     current g, as it does in code called from Go.
//   - arm64: BL (or BLR) to the address, which overwrites R30 (LR), so
//     save it first. On return R25 points to the n slots. Every other
//     general-purpose register is preserved except R27; the flags are
//     clobbered, and so may be the floating-point registers. R28 must
//     hold the current g, as it does in code called from Go.
//
// The entry runs on the goroutine stack below the caller's frame without
// checking the stack bound, using up to a few hundred bytes. Generated
// code that calls it must leave that much room, as a Go function marked
// nosplit must.
//
// The contracts for other architectures are those documented on
// gcWriteBarrier in the runtime's asm_GOARCH.s.
func WriteBarrierEntry(n int) uintptr {
	pc := runtime_writeBarrierEntry(n)
	if pc == 0 {
		panic("goplus: WriteBarrierEntry slot count out of range")
	}
	return pc
}

//go:linkname runtime_writeBarrierFlag
func runtime_writeBarrierFlag() *uint32

//go:linkname runtime_writeBarrierEntry
func runtime_writeBarrierEntry(n int) uintptr
