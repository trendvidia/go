// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// grantedFeatures lists, per vector width, the CPU features archMaxVectorSize
// (midway_amd64.go) has verified before it reports that width as
// allFeatureSize, i.e. before this package runs hardware SIMD at that width.
// The 512 row leans on archsimd's documented bundle: X86.AVX512 is
// F+CD+BW+DQ+VL together and implies AVX and AVX2.
//
// Keep it in step with archMaxVectorSize by hand; TestAVXOnlyEmulates checks
// the runtime side of the same contract.
var grantedFeatures = map[int][]string{
	128: {"AVX", "AVX2", "FMA"},
	256: {"AVX", "AVX2", "FMA", "VPCLMULQDQ"},
	512: {"AVX", "AVX2", "AVX512", "AVX512F", "AVX512CD", "AVX512BW", "AVX512DQ", "AVX512VL", "AVX512VPCLMULQDQ"},
}

// TestBridgeFeatures pins that every archsimd operation this package reaches
// at a vector width needs no CPU feature beyond grantedFeatures[width]. It is
// the static half of trendvidia/go-archive#154: archMaxVectorSize used to grant 128
// bits on bare AVX while the 128-bit bridge went through VPBROADCAST (AVX2)
// and VFMADD (FMA), and nothing in the tree said so until an Ivy Bridge ran
// all.bash. An upstream sync that adds a bridged operation needing more than
// its width's row fails here, on every amd64 host.
//
// What an operation needs comes from its doc comment ("CPU Feature: X"); a
// Go-bodied helper needs whatever the archsimd operations it calls need. A
// bodyless intrinsic with no annotation (the raw Load/Store family, issue
// #97) is taken to need only its width's base instruction set, which is what
// those are; they are counted so a change in their number is visible.
func TestBridgeFeatures(t *testing.T) {
	// No testing.Short() skip on purpose: cmd/dist runs every package test
	// with -short, and a short-skipped pin guards nothing in all.bash.

	fset := token.NewFileSet()
	ctx := build.Default
	ctx.GOARCH = "amd64"
	ctx.BuildTags = append(ctx.BuildTags, "goexperiment.simd")

	src := importer.ForCompiler(fset, "source", nil)
	known := &knownImporter{known: map[string]*types.Package{}, fallback: src}

	arch := checkDir(t, fset, &ctx, "archsimd", "simd/archsimd", known)
	known.known[arch.pkg.Path()] = arch.pkg
	bridge := checkDir(t, fset, &ctx, "internal/bridge", "simd/internal/bridge", known)
	known.known[bridge.pkg.Path()] = bridge.pkg
	midway := checkDir(t, fset, &ctx, ".", "simd", known)

	need := newRequirements(arch)

	var violations []string
	sites := map[int]int{}
	for _, user := range []*checkedPkg{bridge, midway} {
		for id, obj := range user.info.Uses {
			fn, ok := obj.(*types.Func)
			if !ok || fn.Pkg() != arch.pkg {
				continue
			}
			w := width(fn)
			if w == 0 {
				continue // feature accessors and the like
			}
			sites[w]++
			granted, ok := grantedFeatures[w]
			if !ok {
				t.Fatalf("%s: %s is %d bits wide, and grantedFeatures has no row for that", fset.Position(id.Pos()), fn.FullName(), w)
			}
			for _, f := range need.of(fn) {
				if !contains(granted, f) {
					violations = append(violations, fmt.Sprintf("%s: %s needs %s, which archMaxVectorSize does not check before granting %d bits",
						fset.Position(id.Pos()), fn.FullName(), f, w))
				}
			}
		}
	}
	for _, w := range []int{128, 256, 512} {
		if sites[w] == 0 {
			t.Errorf("no archsimd references at %d bits: the walk found nothing to check", w)
		}
	}
	t.Logf("archsimd references checked: 128=%d 256=%d 512=%d; unannotated bodyless intrinsics reached: %d",
		sites[128], sites[256], sites[512], need.unannotated)
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

type checkedPkg struct {
	pkg   *types.Package
	files []*ast.File
	info  *types.Info
}

type knownImporter struct {
	known    map[string]*types.Package
	fallback types.Importer
}

func (k *knownImporter) Import(path string) (*types.Package, error) {
	if p, ok := k.known[path]; ok {
		return p, nil
	}
	return k.fallback.Import(path)
}

// checkDir type-checks the non-test files of dir that build for amd64 with
// GOEXPERIMENT=simd.
func checkDir(t *testing.T, fset *token.FileSet, ctx *build.Context, dir, path string, imp types.Importer) *checkedPkg {
	bp, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, dir+"/"+name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{
		Importer: imp,
		// The bridge and midway files are generated against the arch
		// package's full surface; a stray error here should fail loudly.
		Error: func(err error) { t.Error(err) },
	}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		t.Fatalf("type-checking %s: %v", path, err)
	}
	return &checkedPkg{pkg: pkg, files: files, info: info}
}

var featureRE = regexp.MustCompile(`CPU Features?:?\s*([A-Za-z0-9_]+)`)

// requirements resolves what CPU features an archsimd function needs.
type requirements struct {
	arch        *checkedPkg
	decls       map[*types.Func]*ast.FuncDecl
	memo        map[*types.Func][]string
	unannotated int
}

func newRequirements(arch *checkedPkg) *requirements {
	r := &requirements{arch: arch, decls: map[*types.Func]*ast.FuncDecl{}, memo: map[*types.Func][]string{}}
	for _, f := range arch.files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fn, ok := arch.info.Defs[fd.Name].(*types.Func); ok {
					r.decls[fn] = fd
				}
			}
		}
	}
	return r
}

func (r *requirements) of(fn *types.Func) []string {
	if feats, ok := r.memo[fn]; ok {
		return feats
	}
	r.memo[fn] = nil // cycle guard
	fd := r.decls[fn]
	if fd == nil {
		return nil
	}
	var feats []string
	if fd.Doc != nil {
		if m := featureRE.FindStringSubmatch(fd.Doc.Text()); m != nil {
			feats = []string{m[1]}
		}
	}
	if feats == nil && fd.Body == nil {
		r.unannotated++
	}
	if feats == nil && fd.Body != nil {
		// Trust the body over a missing doc line: whatever it calls, it needs.
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			callee, ok := r.arch.info.Uses[id].(*types.Func)
			if !ok || callee.Pkg() != r.arch.pkg || callee == fn {
				return true
			}
			for _, f := range r.of(callee) {
				if !contains(feats, f) {
					feats = append(feats, f)
				}
			}
			return true
		})
	}
	sort.Strings(feats)
	r.memo[fn] = feats
	return feats
}

var widthRE = regexp.MustCompile(`^(Float|Int|Uint|Mask)(8|16|32|64)x(2|4|8|16|32|64)$`)

// width reports the vector width in bits of a method's receiver or of a
// function's first result, or 0 when neither names a vector type.
func width(fn *types.Func) int {
	sig := fn.Type().(*types.Signature)
	var typ types.Type
	if sig.Recv() != nil {
		typ = sig.Recv().Type()
	} else if sig.Results().Len() > 0 {
		typ = sig.Results().At(0).Type()
	}
	named, ok := typ.(*types.Named)
	if !ok {
		return 0
	}
	m := widthRE.FindStringSubmatch(named.Obj().Name())
	if m == nil {
		return 0
	}
	elem, _ := strconv.Atoi(m[2])
	lanes, _ := strconv.Atoi(m[3])
	return elem * lanes
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}
