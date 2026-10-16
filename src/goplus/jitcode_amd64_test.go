// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import "encoding/binary"

// Hand-assembled amd64 code for the write-barrier tests. Functions are
// entered under ABIInternal: the first two arguments arrive in AX and BX,
// the result returns in AX, and R14 holds g.

type asm []byte

func (a asm) b(bs ...byte) asm { return append(a, bs...) }
func (a asm) u32(v uint32) asm { return binary.LittleEndian.AppendUint32(a, v) }
func (a asm) u64(v uint64) asm { return binary.LittleEndian.AppendUint64(a, v) }

// storeCode returns func(dst *unsafe.Pointer, val unsafe.Pointer) that
// performs *dst = val, recording the store through the write barrier
// when barrier is true, as compiled Go code does.
func storeCode(flag, entry uintptr, barrier bool) []byte {
	var a asm
	if barrier {
		a = a.b(0x49, 0xba).u64(uint64(flag))  // MOVQ $flag, R10
		a = a.b(0x41, 0x83, 0x3a, 0x00)        // CMPL (R10), $0
		a = a.b(0x74, 23)                      // JEQ store
		a = a.b(0x49, 0xba).u64(uint64(entry)) // MOVQ $entry, R10
		a = a.b(0x41, 0xff, 0xd2)              // CALL R10
		a = a.b(0x49, 0x89, 0x1b)              // MOVQ BX, (R11)     new value
		a = a.b(0x4c, 0x8b, 0x10)              // MOVQ (AX), R10     old value
		a = a.b(0x4d, 0x89, 0x53, 0x08)        // MOVQ R10, 8(R11)
	}
	a = a.b(0x48, 0x89, 0x18) // store: MOVQ BX, (AX)
	a = a.b(0xc3)             // RET
	return a
}

// probeRegs are the registers the probe checks: every general-purpose
// register except SP, BP (frame pointer), R11 (the entry's result) and
// R14 (g).
var probeRegs = []byte{0, 1, 2, 3, 6, 7, 8, 9, 10, 12, 13, 15}

// probeCode returns func() uintptr that loads a distinct constant into
// each of probeRegs, calls entry (a 2-slot entry) calls times, storing
// nil into the reserved slots, and returns 0 if every probed register
// still holds its constant, else a nonzero value.
func probeCode(entry uintptr, calls int) []byte {
	var a asm
	for i, r := range probeRegs {
		a = a.b(0x48|r>>3, 0xc7, 0xc0|r&7).u32(0x1000 + uint32(i)) // MOVQ $c, r
	}
	for range calls {
		a = a.b(0x49, 0xbb).u64(uint64(entry)) // MOVQ $entry, R11
		a = a.b(0x41, 0xff, 0xd3)              // CALL R11
		a = a.b(0x49, 0xc7, 0x03).u32(0)       // MOVQ $0, (R11)
		a = a.b(0x49, 0xc7, 0x43, 0x08).u32(0) // MOVQ $0, 8(R11)
	}
	a = a.b(0x45, 0x31, 0xdb) // XORL R11, R11
	for i, r := range probeRegs {
		a = a.b(0x48|r>>3, 0x81, 0xf0|r&7).u32(0x1000 + uint32(i)) // XORQ $c, r
		a = a.b(0x49|(r>>3)<<2, 0x09, 0xc3|(r&7)<<3)               // ORQ r, R11
	}
	a = a.b(0x4c, 0x89, 0xd8) // MOVQ R11, AX
	a = a.b(0xc3)             // RET
	return a
}

// Store-loop variants for BenchmarkWriteBarrierStore.
const (
	loopPlain  = iota // no barrier
	loopInline        // the exported barrier, tested inline
	loopHelper        // a call to a compiled Go helper per store
)

// storeLoopCode returns func(dst *unsafe.Pointer, val unsafe.Pointer, n
// int) that stores val to *dst n times (n > 0). The helper variant calls
// the Go function at helper for each store and, since a Go call may
// clobber every general-purpose register, saves its live registers (dst,
// val and the count) around the call, as a JIT would.
func storeLoopCode(mode int, flag, entry, helper uintptr) []byte {
	var body asm
	switch mode {
	case loopPlain:
		body = body.b(0x48, 0x89, 0x18) // MOVQ BX, (AX)
	case loopInline:
		body = storeCode(flag, entry, true)
		body = body[:len(body)-1] // drop RET
	case loopHelper:
		body = body.b(0x50, 0x53, 0x51)               // PUSHQ AX; PUSHQ BX; PUSHQ CX
		body = body.b(0x49, 0xba).u64(uint64(helper)) // MOVQ $helper, R10
		body = body.b(0x41, 0xff, 0xd2)               // CALL R10
		body = body.b(0x59, 0x5b, 0x58)               // POPQ CX; POPQ BX; POPQ AX
	}
	body = body.b(0x48, 0xff, 0xc9)             // DECQ CX
	body = body.b(0x75, byte(-(len(body) + 2))) // JNE loop
	return body.b(0xc3)                         // RET
}
