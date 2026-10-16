// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !cgo

package testfastleaf_test

import (
	"os"
	"testing"
)

// TestFastLeafNeedsCgo stands in for the fastleaf tests, which all need
// cgo. Without it, a build without cgo leaves the package with no test
// files, which go test reports as a pass. GOPLUS_REQUIRE_CGO marks a run,
// such as a CI lane, that exists to exercise fastleaf, and fails it.
func TestFastLeafNeedsCgo(t *testing.T) {
	if os.Getenv("GOPLUS_REQUIRE_CGO") != "" {
		t.Fatal("built without cgo, so no fastleaf test ran; GOPLUS_REQUIRE_CGO is set")
	}
	t.Skip("the fastleaf tests need cgo")
}
