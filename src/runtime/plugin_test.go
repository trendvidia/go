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
	// name, not split at the first dot after the last '/' (trendvidia/go-archive#27).
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

// TestModuleItabsSitInsideTypesRange pins the go1.27 module layout that
// runtime/type.go's canonicalizeTypeInModule depends on.
//
// Through go1.26 the linker packed only type descriptors into
// [md.types, md.etypes), so "does md's linker own this type descriptor"
// was an address-range test, and canonicalizeTypeInModule used one before
// consulting md.typemap. go1.27 packs the module's itabs into the same
// range -- moduledata gained typedesclen, itaboffset and itabsize, and
// ld/data.go lays out typelinked descriptors, then the rest, then itabs.
// The range test therefore answers yes for an itab address, which is why
// canonicalizeTypeInModule now carries no range test at all and keys
// straight off the (go1.27, pointer-keyed) typemap.
//
// The assertions below are that hazard, stated: the itab block is
// non-empty and lies wholly inside [types, etypes), after the descriptor
// prefix. If this test fails, the layout moved -- go re-read
// canonicalizeTypeInModule before concluding a range test is safe again
// (trendvidia/go-archive#139, port item 4).
func TestModuleItabsSitInsideTypesRange(t *testing.T) {
	types, typedesclen, itaboffset, itabsize, etypes := runtime.FirstModuleTypeRegion()

	if types == 0 || etypes <= types {
		t.Fatalf("first module has no type region: types=%#x etypes=%#x", types, etypes)
	}
	if typedesclen == 0 || typedesclen > etypes-types {
		t.Fatalf("typedesclen=%#x outside [types, etypes) of size %#x", typedesclen, etypes-types)
	}
	// A test binary links the testing package, so it has itabs. A zero
	// here would make the rest of the test vacuous rather than fail it.
	if itabsize == 0 {
		t.Fatal("first module reports no itabs; the layout this pins cannot be checked")
	}
	if itaboffset < typedesclen {
		t.Errorf("itabs start at +%#x, inside the %#x-byte type-descriptor prefix", itaboffset, typedesclen)
	}
	if end := itaboffset + itabsize; end > etypes-types {
		t.Errorf("itabs end at +%#x, past etypes at +%#x", end, etypes-types)
	}
	// The point of the whole test: an address-range test over
	// [types, etypes) matches an itab.
	itab := types + itaboffset
	if !(itab >= types && itab < etypes) {
		t.Errorf("itab address %#x is outside [%#x, %#x); the go1.26 range test would still be sound",
			itab, types, etypes)
	}
}
