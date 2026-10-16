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
package goplus

// JIT is the level of this package's API. It increases by one each time
// a declaration is added.
//
// Level 1: [ObjectExtent].
// Level 2: [SetNoTinyAlloc].
// Level 3: [WriteBarrierFlag], [WriteBarrierEntry].
const JIT = 3
