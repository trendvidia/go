// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"encoding/binary"
	"goplus"
)

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

// frameCode returns func(obj unsafe.Pointer, callee uintptr) uintptr: a
// function with a Go-layout frame that keeps obj at 0(SP) and the
// address of 0(SP) at 8(SP), calls the Go function at callee, and returns
// 8(SP) minus the address of 0(SP), which is 0 if a stack copy during
// the call adjusted that slot. frame and safe describe it for
// goplus.RegisterCode; the two pointer slots are 0 and 8.
func frameCode() (code []byte, frame []goplus.FrameStep, safe uint32) {
	var a asm
	a = a.b(0x48, 0x83, 0xec, 0x18)       // 0:  SUBQ $24, SP
	a = a.b(0x48, 0x89, 0x6c, 0x24, 0x10) // 4:  MOVQ BP, 16(SP)
	a = a.b(0x48, 0x8d, 0x6c, 0x24, 0x10) // 9:  LEAQ 16(SP), BP
	a = a.b(0x48, 0x89, 0x04, 0x24)       // 14: MOVQ AX, (SP)
	a = a.b(0x48, 0x8d, 0x0c, 0x24)       // 18: LEAQ (SP), CX
	a = a.b(0x48, 0x89, 0x4c, 0x24, 0x08) // 22: MOVQ CX, 8(SP)
	a = a.b(0xff, 0xd3)                   // 27: CALL BX
	a = a.b(0x48, 0x8b, 0x44, 0x24, 0x08) // 29: MOVQ 8(SP), AX
	a = a.b(0x48, 0x8d, 0x0c, 0x24)       // 34: LEAQ (SP), CX
	a = a.b(0x48, 0x29, 0xc8)             // 38: SUBQ CX, AX
	a = a.b(0x48, 0x8b, 0x6c, 0x24, 0x10) // 41: MOVQ 16(SP), BP
	a = a.b(0x48, 0x83, 0xc4, 0x18)       // 46: ADDQ $24, SP
	a = a.b(0xc3)                         // 50: RET
	return a, []goplus.FrameStep{{Off: 0, Size: 0}, {Off: 4, Size: 24}, {Off: 50, Size: 0}}, 29
}

// framePointerSlots are frameCode's pointer slots, as SP offsets.
var framePointerSlots = []uint32{0, 8}

// abiIntRegs and abiFloatRegs are ABIInternal's argument and result
// registers on amd64, as documented in package goplus.
var (
	abiIntRegs   = []byte{0, 3, 1, 7, 6, 8, 9, 10, 11} // AX BX CX DI SI R8 R9 R10 R11
	abiFloatRegs = 15                                  // X0-X14
)

// abiProbeCode returns a function that stores each integer argument
// register in order to out[0:9], each of X0-X15 to out[9:25], and returns
// 100+i in the i'th integer result register.
func abiProbeCode(out uintptr) []byte {
	var a asm
	a = a.b(0x49, 0xbd).u64(uint64(out)) // MOVQ $out, R13
	for i, r := range abiIntRegs {
		a = a.b(0x49|(r>>3)<<2, 0x89, 0x85|(r&7)<<3).u32(uint32(8 * i)) // MOVQ r, 8*i(R13)
	}
	for x := range byte(16) {
		a = a.b(0xf2, 0x41|(x>>3)<<2, 0x0f, 0x11, 0x85|(x&7)<<3).u32(uint32(8 * (9 + int(x)))) // MOVSD Xx, ...(R13)
	}
	for i, r := range abiIntRegs {
		a = a.b(0x48|r>>3, 0xc7, 0xc0|r&7).u32(uint32(100 + i)) // MOVQ $100+i, r
	}
	return a.b(0xc3)
}

type abiProbeFunc = func(a0, a1, a2, a3, a4, a5, a6, a7, a8 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14 float64) (r0, r1, r2, r3, r4, r5, r6, r7, r8 uintptr)

func callABIProbe(f abiProbeFunc) []uintptr {
	r0, r1, r2, r3, r4, r5, r6, r7, r8 := f(1, 2, 3, 4, 5, 6, 7, 8, 9,
		0.5, 1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5, 11.5, 12.5, 13.5, 14.5)
	return []uintptr{r0, r1, r2, r3, r4, r5, r6, r7, r8}
}

// spinCode returns func() that increments *counter until *stop is
// nonzero. With poll, each iteration compares SP with the stack guard
// and calls yield when it is not above it; without, the loop has no
// point at which the runtime can stop it. frame and safe describe it for
// goplus.RegisterCode (no pointer slots: counter and stop are globals).
func spinCode(counter, stop, yield uintptr, poll bool) (code []byte, frame []goplus.FrameStep, safe uint32) {
	var a asm
	a = a.b(0x48, 0x83, 0xec, 0x08)          // 0:  SUBQ $8, SP
	a = a.b(0x48, 0x89, 0x2c, 0x24)          // 4:  MOVQ BP, (SP)
	a = a.b(0x48, 0x8d, 0x2c, 0x24)          // 8:  LEAQ (SP), BP
	a = a.b(0x49, 0xbc).u64(uint64(counter)) // 12: reload: MOVQ $counter, R12
	a = a.b(0x49, 0xbd).u64(uint64(stop))    // 22: MOVQ $stop, R13
	a = a.b(0x49, 0xff, 0x04, 0x24)          // 32: loop: INCQ (R12)
	a = a.b(0x41, 0x83, 0x7d, 0x00, 0x00)    // 36: CMPL (R13), $0
	a = a.b(0x75, 0)                         // 41: JNE done (patched below)
	jne := len(a) - 1
	if poll {
		a = a.b(0x49, 0x3b, 0x66, byte(goplus.StackGuardOffset())) // 43: CMPQ SP, StackGuardOffset(R14)
		a = a.b(0x77, byte(32-(len(a)+2)))                         // 47: JHI loop
		a = a.b(0x48, 0xb8).u64(uint64(yield))                     // 49: MOVQ $yield, AX
		a = a.b(0xff, 0xd0)                                        // 59: CALL AX
		safe = uint32(len(a))                                      // 61
		a = a.b(0xeb, byte(12-(len(a)+2)))                         // 61: JMP reload
	} else {
		a = a.b(0xeb, byte(32-(len(a)+2))) // JMP loop
	}
	a[jne] = byte(len(a) - (jne + 1))
	a = a.b(0x48, 0x8b, 0x2c, 0x24) // done: MOVQ (SP), BP
	a = a.b(0x48, 0x83, 0xc4, 0x08) // ADDQ $8, SP
	ret := uint32(len(a))
	a = a.b(0xc3) // RET
	return a, []goplus.FrameStep{{Off: 0, Size: 0}, {Off: 4, Size: 8}, {Off: ret, Size: 0}}, safe
}
