// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package goplus is the go-plus toolchain's runtime API for code that
// cooperates with Go's runtime from outside the compiler: a virtual
// machine whose guest values are ordinary Go memory, and the machine
// code it generates at run time.
//
// The package exists only in the go-plus fork (github.com/trendvidia/go;
// `go version` reports "(plus …)"). A program that imports it does not
// build with a stock toolchain, which is the intent: a dependency on
// these hooks fails at compile time, never at run time.
//
// The API is versioned by [JIT]. A consumer that needs a given level
// asserts it at compile time, for example
//
//	const _ = uint(goplus.JIT - 1) // needs level 1 or later
//
// so a toolchain whose API is older fails to build. A change that is not
// backward compatible renames the affected declaration rather than
// changing it in place.
//
// # Calling Go from generated code
//
// Generated code that calls a Go function, or is called by one through a
// func value, uses Go's internal register calling convention,
// ABIInternal. Go does not promise it is stable; this documents it as of
// this release, and the package's tests fail if it changes.
//
// On amd64:
//
//   - integer and pointer arguments and results: AX, BX, CX, DI, SI, R8,
//     R9, R10, R11, in that order; floating-point: X0 to X14;
//   - R14 holds the current goroutine (g) and X15 is zero, on entry and
//     on return, and must still hold them at any call into Go;
//   - DX holds the closure context of a func value call;
//   - SP, BP (the frame pointer) and R14 are preserved by a Go callee;
//     every other register may be clobbered.
//
// On arm64:
//
//   - integer and pointer arguments and results: R0 to R15; floating-point:
//     F0 to F15;
//   - R28 holds the current goroutine (g), and must still hold it at any
//     call into Go;
//   - R26 holds the closure context of a func value call;
//   - R18 is reserved for the platform, R16, R17 and R27 are scratch, R29
//     is the frame pointer and R30 the link register;
//   - SP, R28 and R29 are preserved by a Go callee; every other register
//     may be clobbered.
//
// Arguments that do not fit in registers are passed on the stack, and the
// caller reserves spill space for the register arguments at the bottom of
// its frame; see [Func].
package goplus

// JIT is the level of this package's API. It increases by one each time
// a declaration is added.
//
// Level 1: [ObjectExtent].
// Level 2: [SetNoTinyAlloc].
// Level 3: [WriteBarrierFlag], [WriteBarrierEntry].
// Level 4: [RegisterCode] and its types.
// Level 5: [StackGuardOffset], [YieldEntry].
// Level 6: [AllocDomain].
// Level 7: [Layout].
// Level 8: [AllocDomain.SetBudget], [ErrAllocBudget].
// Level 9: [SampleTag].
// Level 10: [AllocDomain.SetBudget] takes [math.MaxUint64] for no budget;
// 0 is a budget with nothing left, where before it removed the budget.
// Level 11: an [AllocDomain]'s budget refuses only allocations made inside
// [AllocDomain.Run]; goroutines a call starts are counted, never refused.
// Level 12: an [iter.Pull] iterator's body is refused while the goroutine
// that called next or stop is inside Run on the body's domain; at level
// 11 it never was, and before level 11 it always was.
// Level 13: [Release], [Revision]. Every binary the go command builds
// also records them as the build settings "goplus" and "goplus.revision",
// which [runtime/debug.ReadBuildInfo] returns without importing this
// package and `go version -m` prints. [runtime.Version] is unchanged.
const JIT = 13
