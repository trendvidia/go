// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"internal/testenv"
	"path/filepath"
	"strings"
	"testing"
)

// TestFeatureGuardsChecker exercises the checker against sources whose answer
// is known. TestFeatureGuards itself only ever runs against a tree where it
// passes, so the states that matter -- a missing guard, a guard that does not
// imply what it looks like it implies, a requirement seen more than once, a
// declaration whose requirement cannot be computed -- are never reached from
// it. Every defect this file pins was invisible to that run.
func TestFeatureGuardsChecker(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	fset := token.NewFileSet()
	ctx := build.Default
	ctx.GOARCH = "amd64"
	ctx.BuildTags = append(ctx.BuildTags, "goexperiment.simd")
	arch := typeCheckDir(t, fset, &ctx, filepath.Join("..", ".."), "simd/archsimd", false, nil)
	graph := featureImplications(t, arch)
	need := requiredFeatures(arch, graph)

	for _, tc := range []struct {
		name string
		body string
		// want lists a substring of each expected violation, in the order
		// walk sorts them; an empty want means the source must pass.
		want                []string
		checked, unresolved int
	}{{
		name:    "unguarded emulated op",
		body:    `func TestF(t *testing.T) { sinkFixture = u8.Less(u8) }`,
		want:    []string{"Uint8x16).Less needs AVX2"},
		checked: 1,
	}, {
		name:    "baseline covers AVX",
		body:    `func TestF(t *testing.T) { sinkFixture = u8.Add(u8) }`,
		checked: 1,
	}, {
		name:    "AVX2 block",
		body:    `func TestF(t *testing.T) { if archsimd.X86.AVX2() { sinkFixture = u8.Less(u8) } }`,
		checked: 1,
	}, {
		name: "skip preamble",
		body: `func TestF(t *testing.T) {
			if !archsimd.X86.AVX2() { t.Skip("no AVX2") }
			sinkFixture = u8.Less(u8)
		}`,
		checked: 1,
	}, {
		name:    "AVX512 implies AVX2",
		body:    `func TestF(t *testing.T) { if archsimd.X86.AVX512() { sinkFixture = u8.Less(u8) } }`,
		checked: 1,
	}, {
		// archsimd's X86Features.AVX512VPCLMULQDQ doc deliberately omits the
		// "also supports AVX, AVX2, and AVX512" line every other AVX512*
		// accessor carries: cpu_x86.go sets it under AVX512F alone. A guard
		// on it must not be taken to establish AVX2.
		name:    "AVX512VPCLMULQDQ does not imply AVX2",
		body:    `func TestF(t *testing.T) { if archsimd.X86.AVX512VPCLMULQDQ() { sinkFixture = u8.Less(u8) } }`,
		want:    []string{"Uint8x16).Less needs AVX2"},
		checked: 1,
	}, {
		// The other direction: AVXVNNI is documented to imply AVX and AVX2,
		// so guarding on it is enough and must not be a false violation.
		name:    "AVXVNNI implies AVX2",
		body:    `func TestF(t *testing.T) { if archsimd.X86.AVXVNNI() { sinkFixture = u8.Less(u8) } }`,
		checked: 1,
	}, {
		name: "helper requirement surfaces at the call site",
		body: `func helperF() { sinkFixture = u8.Less(u8) }
		func TestF(t *testing.T) { helperF() }`,
		want:    []string{"simd_test.helperF needs AVX2"},
		checked: 1,
	}, {
		// One unguarded helper is one violation however many times the
		// helper reaches the same op; archsimd wrappers reach one op a dozen
		// times, so a duplicated requirement floods the log.
		name: "repeated requirement reported once",
		body: `func helperF() {
			sinkFixture = u8.Less(u8)
			sinkFixture = u8.Less(u8)
			sinkFixture = u8.Less(u8)
		}
		func TestF(t *testing.T) { helperF() }`,
		want:    []string{"simd_test.helperF needs AVX2"},
		checked: 1,
	}, {
		// Benchmarks run in the same binary, under the same AVX-only
		// TestMain, and SIGILL the same way.
		name:    "benchmark is checked",
		body:    `func BenchmarkF(b *testing.B) { sinkFixture = u8.Less(u8) }`,
		want:    []string{"Uint8x16).Less needs AVX2"},
		checked: 1,
	}, {
		// LoadInt8x64 is bodyless and unannotated: its requirement cannot be
		// computed, and calling it must not be counted as a checked
		// reference.
		name:       "unannotated intrinsic counted as unresolved",
		body:       `func TestF(t *testing.T) { sinkFixture = archsimd.LoadInt8x64(&i8arr) }`,
		unresolved: 1,
	}, {
		// A wrapper over an unresolved declaration is unresolved too, rather
		// than confidently "needs nothing", but the requirements it does
		// know are still enforced.
		name: "unknown propagates but does not swallow known features",
		body: `func helperF() {
			sinkFixture = archsimd.LoadInt8x64(&i8arr)
			sinkFixture = u8.Less(u8)
		}
		func TestF(t *testing.T) { helperF() }`,
		want:       []string{"simd_test.helperF needs AVX2"},
		unresolved: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			res := checkFixture(t, fset, arch, graph, need, tc.body)
			if len(res.violations) != len(tc.want) {
				t.Fatalf("got %d violations, want %d:\n%s",
					len(res.violations), len(tc.want), strings.Join(res.violations, "\n"))
			}
			for i, want := range tc.want {
				if !strings.Contains(res.violations[i], want) {
					t.Errorf("violation %d = %q, want it to mention %q", i, res.violations[i], want)
				}
			}
			if res.checked != tc.checked || res.unresolved != tc.unresolved {
				t.Errorf("checked=%d unresolved=%d, want checked=%d unresolved=%d",
					res.checked, res.unresolved, tc.checked, tc.unresolved)
			}
		})
	}
}

const fixturePreamble = `package simd_test

import (
	"testing"

	"simd/archsimd"
)

var sinkFixture any
var u8 archsimd.Uint8x16
var i8arr [64]int8

`

// checkFixture runs the guard checker over one synthetic test file, type
// checked against the real archsimd.
func checkFixture(t *testing.T, fset *token.FileSet, arch *checkedPkg, graph featureGraph, need map[types.Object]requirement, body string) walkResult {
	t.Helper()
	f, err := parser.ParseFile(fset, "fixture_test.go", fixturePreamble+body+"\n", parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg := typeCheckFiles(t, fset, "simd/archsimd/internal/simd_test", []*ast.File{f}, arch.pkg)
	return newResolver(fset, graph, need, pkg).walk(pkg)
}

// TestFeatureGuardsReduce pins the set arithmetic. reduce only ever sees a
// non-empty set when something is unguarded, which is exactly the run
// TestFeatureGuards never has.
func TestFeatureGuardsReduce(t *testing.T) {
	// A graph with the shape archsimd documents, written out so this test
	// does not depend on the toolchain being available.
	g := featureGraph{
		"AVX":     {},
		"AVX2":    {"AVX": true},
		"AVX512":  {"AVX": true, "AVX2": true, "AVX512F": true, "AVX512BW": true},
		"AVX512F": {"AVX": true, "AVX2": true, "AVX512": true, "AVX512BW": true},
		"FMA":     {"AVX": true},
	}
	for _, tc := range []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"AVX2"}, []string{"AVX2"}},
		{[]string{"AVX2", "AVX2"}, []string{"AVX2"}},
		{[]string{"AVX2", "AVX2", "AVX2", "FMA", "FMA"}, []string{"AVX2", "FMA"}},
		{[]string{"AVX", "AVX2"}, []string{"AVX2"}},
		{[]string{"AVX2", "AVX"}, []string{"AVX2"}},
		{[]string{"AVX2", "AVX512"}, []string{"AVX512"}},
		// Two spellings of one feature imply each other: keeping neither
		// would erase the requirement entirely.
		{[]string{"AVX512", "AVX512F"}, []string{"AVX512"}},
		{[]string{"AVX512F", "AVX512"}, []string{"AVX512F"}},
		{[]string{"AVX2", "FMA"}, []string{"AVX2", "FMA"}},
	} {
		got := g.reduce(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("reduce(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
