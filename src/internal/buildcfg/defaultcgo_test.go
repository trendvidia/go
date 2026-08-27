// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package buildcfg

import "testing"

// TestDefaultCGOEnabledUnset pins the fork's build requirement that the
// toolchain is built with CGO_ENABLED unset (README, "Building this fork").
// cmd/dist bakes $CGO_ENABLED at make.bash time into DefaultCGO_ENABLED;
// a non-empty value overrides cmd/go's platform default and, for "1",
// disables the upstream "no C compiler in PATH -> default cgo off"
// autodetection.
func TestDefaultCGOEnabledUnset(t *testing.T) {
	if DefaultCGO_ENABLED != "" {
		t.Errorf(`DefaultCGO_ENABLED = %q, want "": this toolchain was built with CGO_ENABLED=%[1]q exported; `+
			`rebuild with CGO_ENABLED unset (Unix: env -u CGO_ENABLED ./make.bash; `+
			`Windows: set "CGO_ENABLED=" && make.bat)`, DefaultCGO_ENABLED)
	}
}
