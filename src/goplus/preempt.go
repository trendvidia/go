// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import (
	"internal/abi"
	"internal/goarch"
)

// StackGuardOffset returns the offset, in the goroutine descriptor the g
// register points to (R14 on amd64, R28 on arm64), of the word compiled
// Go code compares the stack pointer with in every function prologue.
//
// The runtime asks a goroutine to stop, for a collection, a
// stop-the-world or to let another goroutine run, by raising that word
// above any stack pointer. It cannot stop a goroutine inside generated
// code, which has no point safe for asynchronous preemption, so a loop in
// generated code must poll: at each back-edge, compare the stack pointer
// with the word and, if the stack pointer is not above it (unsigned),
// call [YieldEntry]. On amd64:
//
//	CMPQ SP, 16(R14)    // 16 is StackGuardOffset()
//	JHI  loop
//	CALL yield
//
// and on arm64:
//
//	MOVD 16(R28), R1    // 16 is StackGuardOffset()
//	CMP  R1, RSP
//	BHI  loop
//	BL   yield
//
// The function's frame must be registered with [RegisterCode], with the
// call to the yield entry declared as a safe point, because the runtime
// scans or copies the stack while the goroutine is stopped there.
func StackGuardOffset() uintptr {
	return 2 * goarch.PtrSize
}

// YieldEntry returns the address of a Go function generated code calls
// when its preemption poll (see [StackGuardOffset]) fires. It takes no
// arguments and returns nothing, under ABIInternal: like any call into
// Go, it may clobber every register the package documentation does not
// list as preserved, so live values must be in the frame, pointers among
// them declared at the call's safe point.
//
// It stops the goroutine if the runtime asked for that, and otherwise
// returns at once. If the poll fired because the stack is close to its
// limit rather than for preemption, it grows the stack, copying the
// generated frame.
func YieldEntry() uintptr {
	return abi.FuncPCABIInternal(yield)
}

// yield's prologue does the work: like every Go function that is not a
// leaf, it compares the stack pointer with the stack guard, and if the
// runtime raised the guard to ask for a stop it enters the runtime
// through morestack, which stops the goroutine or grows its stack. The
// call to yieldBody keeps yield from being a leaf, which would let the
// compiler drop that check.
//
//go:noinline
func yield() { yieldBody() }

//go:noinline
func yieldBody() {}
