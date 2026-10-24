// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package runtime_test

import (
	"runtime"
	"testing"
)

// TestGoplusMaxStackCgo checks that a goplus.AllocDomain stack limit
// refuses a recursion through cgo callbacks, that the refusal's panic
// unwinds through the C frames to Run's caller, and that the goroutine
// can then call C and be refused again.
func TestGoplusMaxStackCgo(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("SetMaxStack is not enforced on %s", runtime.GOARCH)
	}
	got := runTestProg(t, "testprogcgo", "GoplusMaxStackCgo")
	if want := "OK\n"; got != want {
		t.Fatalf("expected %q, but got:\n%s", want, got)
	}
}
