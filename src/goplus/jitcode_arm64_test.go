// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"encoding/binary"
	"goplus"
)

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

// frameCode returns func(obj unsafe.Pointer, callee uintptr) uintptr: a
// function with a Go-layout frame that keeps obj at 8(SP) and the
// address of 8(SP) at 16(SP), calls the Go function at callee, and
// returns 16(SP) minus the address of 8(SP), which is 0 if a stack copy
// during the call adjusted that slot. frame and safe describe it for
// goplus.RegisterCode; the two pointer slots are 8 and 16.
func frameCode() (code []byte, frame []goplus.FrameStep, safe uint32) {
	var a asm
	a = a.i(0xf81e0ffe) // 0:  STR X30, [SP, #-32]!
	a = a.i(0xf81f83fd) // 4:  STUR X29, [SP, #-8]
	a = a.i(0xd10023fd) // 8:  SUB X29, SP, #8
	a = a.i(0xf90007e0) // 12: STR X0, [SP, #8]
	a = a.i(0x910023f0) // 16: ADD X16, SP, #8
	a = a.i(0xf9000bf0) // 20: STR X16, [SP, #16]
	a = a.i(0xd63f0020) // 24: BLR X1
	a = a.i(0xf9400be0) // 28: LDR X0, [SP, #16]
	a = a.i(0x910023e1) // 32: ADD X1, SP, #8
	a = a.i(0xcb010000) // 36: SUB X0, X0, X1
	a = a.i(0xf85f83fd) // 40: LDUR X29, [SP, #-8]
	a = a.i(0xf84207fe) // 44: LDR X30, [SP], #32
	a = a.i(insRet)     // 48: RET
	return a, []goplus.FrameStep{{Off: 0, Size: 0}, {Off: 4, Size: 32}, {Off: 48, Size: 0}}, 28
}

// framePointerSlots are frameCode's pointer slots, as SP offsets.
var framePointerSlots = []uint32{8, 16}

// abiIntRegs and abiFloatRegs are ABIInternal's argument and result
// registers on arm64, as documented in package goplus.
var (
	abiIntRegs   = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	abiFloatRegs = 16 // F0-F15
)

// abiProbeCode returns a function that stores each integer argument
// register in order to out[0:16], each of F0-F15 to out[16:32], and
// returns 100+i in the i'th integer result register.
func abiProbeCode(out uintptr) []byte {
	var a asm
	a = a.mov64(16, uint64(out))
	for i, r := range abiIntRegs {
		a = a.i(0xf9000000 | uint32(i)<<10 | 16<<5 | uint32(r)) // STR Xr, [X16, #8*i]
	}
	for x := range uint32(16) {
		a = a.i(0xfd000000 | (16+x)<<10 | 16<<5 | x) // STR Dx, [X16, #8*(16+x)]
	}
	for i, r := range abiIntRegs {
		a = a.i(0xd2800000 | uint32(100+i)<<5 | uint32(r)) // MOVZ Xr, #100+i
	}
	return a.i(insRet)
}

type abiProbeFunc = func(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14, f15 float64) (r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15 uintptr)

func callABIProbe(f abiProbeFunc) []uintptr {
	r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15 := f(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		0.5, 1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5, 11.5, 12.5, 13.5, 14.5, 15.5)
	return []uintptr{r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15}
}

// spinCode returns func() that increments *counter until *stop is
// nonzero. With poll, each iteration compares SP with the stack guard
// and calls yield when it is not above it; without, the loop has no
// point at which the runtime can stop it. frame and safe describe it for
// goplus.RegisterCode (no pointer slots: counter and stop are globals).
func spinCode(counter, stop, yield uintptr, poll bool) (code []byte, frame []goplus.FrameStep, safe uint32) {
	var a asm
	a = a.i(insPushLR)  // 0: STR X30, [SP, #-16]!
	a = a.i(0xf81f83fd) // 4: STUR X29, [SP, #-8]
	a = a.i(0xd10023fd) // 8: SUB X29, SP, #8
	reload := len(a) / 4
	a = a.mov64(20, uint64(counter))
	a = a.mov64(21, uint64(stop))
	loop := len(a) / 4
	a = a.i(0xf9400296) // LDR X22, [X20]
	a = a.i(0x910006d6) // ADD X22, X22, #1
	a = a.i(0xf9000296) // STR X22, [X20]
	a = a.i(0xb94002b7) // LDR W23, [X21]
	cbnz := len(a) / 4
	a = a.i(0) // CBNZ W23, done (patched below)
	if poll {
		a = a.i(0xf9400000 | uint32(goplus.StackGuardOffset()/8)<<10 | 28<<5 | 24) // LDR X24, [X28, #StackGuardOffset]
		a = a.i(0x910003f9)                                                        // MOV X25, SP
		a = a.i(0xeb18033f)                                                        // CMP X25, X24
		a = a.i(0x54000008 | uint32(loop-len(a)/4)&0x7ffff<<5)                     // B.HI loop
		a = a.mov64(16, uint64(yield))
		a = a.i(insBLRX16)
		safe = uint32(len(a))
		a = a.i(0x14000000 | uint32(reload-len(a)/4)&0x3ffffff) // B reload
	} else {
		a = a.i(0x14000000 | uint32(loop-len(a)/4)&0x3ffffff) // B loop
	}
	done := len(a) / 4
	binary.LittleEndian.PutUint32(a[cbnz*4:], 0x35000000|uint32(done-cbnz)<<5|23)
	a = a.i(0xf85f83fd) // done: LDUR X29, [SP, #-8]
	a = a.i(insPopLR)   // LDR X30, [SP], #16
	ret := uint32(len(a))
	a = a.i(insRet)
	return a, []goplus.FrameStep{{Off: 0, Size: 0}, {Off: 4, Size: 16}, {Off: ret, Size: 0}}, safe
}
