// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !arm64

package runtime

// goplusHijackReturnPC is never called: the stack limit is enforced on
// amd64 and arm64 only (goplusRefuseStack).
func goplusHijackReturnPC() uintptr {
	throw("goplusHijackReturnPC")
	return 0
}
