// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || arm64

package runtime

import "internal/abi"

// goplusHijackReturn is where a function whose return address
// goplusRaiseMaxStackLate overwrote returns to, with the registers of its
// return and the stack of its caller. It clears gp.goplusHijack, and
// unless gp can no longer take a panic (goplusCanPanic: the call took a
// lock, say), continues as a call from the caller at the return address
// to goplusPanicMaxStack; otherwise it jumps to the return address.
// Implemented in assembly.
func goplusHijackReturn()

func goplusHijackReturnPC() uintptr {
	return abi.FuncPCABI0(goplusHijackReturn)
}
