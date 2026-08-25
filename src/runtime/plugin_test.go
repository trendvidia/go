// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
)

var pkgFromInitNameTests = []struct {
	name string
	want string
}{
	// Not an init-function symbol: returned whole.
	{"", ""},
	{"main.main", "main.main"},

	{"runtime.init", "runtime"},
	{"runtime.init.7", "runtime"},
	{"testplugin/common.init.0", "testplugin/common"},
	{"init.init.0", "init"},

	// Dotted import paths: the suffix is stripped from the END of the
	// name, not split at the first dot after the last '/' (#27).
	{"gopkg.in/yaml.v2.init", "gopkg.in/yaml.v2"},
	{"gopkg.in/yaml.v2.init.0", "gopkg.in/yaml.v2"},
	{"x.initfoo.init.0", "x.initfoo"},

	// A ".init" that is part of the package path must not match.
	{"a/b.init2/c.init.1", "a/b.init2/c"},
}

func TestPkgFromInitName(t *testing.T) {
	for _, tt := range pkgFromInitNameTests {
		if got := runtime.PkgFromInitName(tt.name); got != tt.want {
			t.Errorf("runtime.PkgFromInitName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
