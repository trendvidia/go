// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import "encoding/binary"

// Hand-assembled arm64 code for the write-barrier tests. Functions are
// entered under ABIInternal: the first two arguments arrive in R0 and R1,
// the result returns in R0, and R28 holds g.

type asm []byte

func (a asm) i(ins ...uint32) asm {
	for _, v := range ins {
		a = binary.LittleEndian.AppendUint32(a, v)
	}
	return a
}

// mov64 loads v into Xd with MOVZ and three MOVKs.
func (a asm) mov64(d uint32, v uint64) asm {
	a = a.i(0xd2800000 | uint32(v&0xffff)<<5 | d) // MOVZ Xd, #v[0:16]
	for hw := uint32(1); hw < 4; hw++ {
		a = a.i(0xf2800000 | hw<<21 | uint32(v>>(16*hw)&0xffff)<<5 | d) // MOVK Xd, #v[16*hw:], LSL #16*hw
	}
	return a
}

const (
	insPushLR = 0xf81f0ffe // STR X30, [SP, #-16]!
	insPopLR  = 0xf84107fe // LDR X30, [SP], #16
	insRet    = 0xd65f03c0 // RET
	insBLRX16 = 0xd63f0200 // BLR X16
)

// storeCode returns func(dst *unsafe.Pointer, val unsafe.Pointer) that
// performs *dst = val, recording the store through the write barrier
// when barrier is true, as compiled Go code does.
func storeCode(flag, entry uintptr, barrier bool) []byte {
	var a asm
	a = a.i(insPushLR) // the entry's BL overwrites LR
	if barrier {
		a = a.mov64(16, uint64(flag))
		a = a.i(0xb9400211)             // LDR W17, [X16]
		a = a.i(0x34000000 | 9<<5 | 17) // CBZ W17, store (9 instructions on)
		a = a.mov64(16, uint64(entry))
		a = a.i(insBLRX16)
		a = a.i(0xf9000321) // STR X1, [X25]        new value
		a = a.i(0xf9400011) // LDR X17, [X0]        old value
		a = a.i(0xf9000731) // STR X17, [X25, #8]
	}
	a = a.i(0xf9000001) // store: STR X1, [X0]
	a = a.i(insPopLR, insRet)
	return a
}

// probeRegs are the registers the probe checks: every general-purpose
// register except R16 and R17 (the probe's scratch), R18 (platform),
// R25 (the entry's result), R27 (clobbered by contract), R28 (g), R29
// (frame pointer) and R30 (LR).
var probeRegs = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 19, 20, 21, 22, 23, 24, 26}

// probeCode returns func() uintptr that loads a distinct constant into
// each of probeRegs, calls entry (a 2-slot entry) calls times, storing
// nil into the reserved slots, and returns 0 if every probed register
// still holds its constant, else a nonzero value.
func probeCode(entry uintptr, calls int) []byte {
	var a asm
	a = a.i(insPushLR)
	for i, r := range probeRegs {
		a = a.i(0xd2800000 | (0x1000+uint32(i))<<5 | uint32(r)) // MOVZ Xr, #c
	}
	for range calls {
		a = a.mov64(16, uint64(entry))
		a = a.i(insBLRX16)
		a = a.i(0xf900033f) // STR XZR, [X25]
		a = a.i(0xf900073f) // STR XZR, [X25, #8]
	}
	a = a.i(0xd2800011) // MOVZ X17, #0
	for i, r := range probeRegs {
		a = a.i(0xd2800010 | (0x1000+uint32(i))<<5) // MOVZ X16, #c
		a = a.i(0xca100010 | uint32(r)<<5)          // EOR X16, Xr, X16
		a = a.i(0xaa100231)                         // ORR X17, X17, X16
	}
	a = a.i(0xaa1103e0) // MOV X0, X17
	a = a.i(insPopLR, insRet)
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
		body = body.i(0xf9000001) // STR X1, [X0]
	case loopInline:
		body = storeCode(flag, entry, true)
		body = body[4 : len(body)-8] // drop the LR push, and the LR pop and RET
	case loopHelper:
		body = body.i(0xa9bf07e0) // STP (X0, X1), [SP, #-16]!
		body = body.i(0xf81f0fe2) // STR X2, [SP, #-16]!
		body = body.mov64(16, uint64(helper))
		body = body.i(insBLRX16)
		body = body.i(0xf84107e2) // LDR X2, [SP], #16
		body = body.i(0xa8c107e0) // LDP (X0, X1), [SP], #16
	}
	body = body.i(0xf1000442) // SUBS X2, X2, #1
	n := len(body) / 4
	body = body.i(0x54000001 | uint32(-n)&0x7ffff<<5) // B.NE loop
	return asm(nil).i(insPushLR).b(body...).i(insPopLR, insRet)
}

func (a asm) b(bs ...byte) asm { return append(a, bs...) }
