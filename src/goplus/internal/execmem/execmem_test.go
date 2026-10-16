// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package execmem_test

import (
	"goplus/internal/execmem"
	"runtime"
	"testing"
)

// addOne is func(x uintptr) uintptr { return x + 1 } under ABIInternal.
var addOne = map[string][]byte{
	"amd64": {0x48, 0x8d, 0x40, 0x01, 0xc3},                   // LEAQ 1(AX), AX; RET
	"arm64": {0x00, 0x04, 0x00, 0x91, 0xc0, 0x03, 0x5f, 0xd6}, // ADD X0, X0, #1; RET
}

func TestMapAndCall(t *testing.T) {
	if !execmem.Supported() {
		t.Skipf("unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	c, err := execmem.Map(addOne[runtime.GOARCH])
	if err != nil {
		t.Fatal(err)
	}
	f := execmem.Func[func(uintptr) uintptr](c)
	for _, x := range []uintptr{0, 41, 1 << 30} {
		if got := f(x); got != x+1 {
			t.Fatalf("generated addOne(%d) = %d", x, got)
		}
	}
	if err := c.Unmap(); err != nil {
		t.Fatal(err)
	}
}
