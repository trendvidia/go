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
// refuses a recursion through cgo callbacks, each of whose growths comes
// at the entry of a callback the runtime calls (trendvidia/go#100), and
// that the refusal's panic unwinds through the C frames to Run's caller.
func TestGoplusMaxStackCgo(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("SetMaxStack is not enforced on %s", runtime.GOARCH)
	}
	got := runTestProg(t, "testprogcgo", "GoplusMaxStackCgo")
	if want := "OK\n"; got != want {
		t.Fatalf("expected %q, but got:\n%s", want, got)
	}
}
