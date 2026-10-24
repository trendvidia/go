// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"internal/testenv"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestGoplusMapWriter checks that a goplus.AllocDomain budget recognises
// every function that marks a map as being written, so that it never
// refuses an allocation made while one runs (trendvidia/go#96). It reads
// the functions from the source of internal/runtime/maps: one that sets
// the mark and that goplusMapWriter does not name is an allocation the
// budget could refuse mid-write, leaving the map to throw "concurrent map
// writes" at its next write.
func TestGoplusMapWriter(t *testing.T) {
	testenv.MustHaveSource(t)
	dir := filepath.Join(testenv.GOROOT(t), "src", "internal", "runtime", "maps")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		linknames := map[string]string{}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if fields := strings.Fields(c.Text); len(fields) == 3 && fields[0] == "//go:linkname" {
					linknames[fields[1]] = fields[2]
				}
			}
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !setsWriting(fd.Body) {
				continue
			}
			writers = append(writers, mapsSymbol(fd, linknames))
		}
	}
	// Positive marker: the source was read and the mark found where it
	// is known to be set.
	for _, want := range []string{"runtime.mapassign", "runtime.mapassign_faststr", "internal/runtime/maps.(*Map).Clear"} {
		if !slices.Contains(writers, want) {
			t.Fatalf("found map writers %q in %s, want them to include %s", writers, dir, want)
		}
	}
	for _, name := range writers {
		if !runtime.GoplusMapWriter(name) {
			t.Errorf("%s marks a map as being written, but goplusMapWriter does not name it", name)
		}
	}

	// And it names nothing that only reads a map.
	for _, name := range []string{
		"runtime.mapaccess1",
		"runtime.mapaccess2_faststr",
		"runtime.makemap",
		"internal/runtime/maps.(*Map).Get",
		"internal/runtime/maps.(*Map).Clone",
		"main.mapassign",
	} {
		if runtime.GoplusMapWriter(name) {
			t.Errorf("goplusMapWriter(%q) = true, want false", name)
		}
	}
}

// setsWriting reports whether body changes a map's writing mark, as in
// m.writing ^= 1, by any assignment or increment.
func setsWriting(body *ast.BlockStmt) bool {
	found := false
	isWriting := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "writing"
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			found = found || slices.ContainsFunc(s.Lhs, isWriting)
		case *ast.IncDecStmt:
			found = found || isWriting(s.X)
		}
		return !found
	})
	return found
}

// mapsSymbol returns the symbol name of fd, a function of package
// internal/runtime/maps: the target of its linkname directive in
// linknames, if it has one, or the name the compiler gives it. A method
// of a generic type is named with its type parameters as [...], which
// names no instance, so the test reports it rather than passing it.
func mapsSymbol(fd *ast.FuncDecl, linknames map[string]string) string {
	if fd.Recv == nil {
		if target, ok := linknames[fd.Name.Name]; ok {
			return target
		}
	}
	const pkg = "internal/runtime/maps."
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return pkg + fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	ptr := false
	if star, ok := recv.(*ast.StarExpr); ok {
		ptr, recv = true, star.X
	}
	typ := "?"
	switch r := recv.(type) {
	case *ast.Ident:
		typ = r.Name
	case *ast.IndexExpr:
		if id, ok := r.X.(*ast.Ident); ok {
			typ = id.Name + "[...]"
		}
	case *ast.IndexListExpr:
		if id, ok := r.X.(*ast.Ident); ok {
			typ = id.Name + "[...]"
		}
	}
	if ptr {
		return pkg + "(*" + typ + ")." + fd.Name.Name
	}
	return pkg + typ + "." + fd.Name.Name
}

// TestGoplusInMapWrite checks the walk that finds a map write above an
// allocation (trendvidia/go#96), on stacks of symbol names: it goes
// through runtime code, the stateless helpers the stack limit excludes,
// and the hash and equality functions the compiler generates, and ends at
// the first frame of any other code. No allocation lands under a helper
// or a generated function during a map write today, so only such stacks
// reach those cases.
func TestGoplusInMapWrite(t *testing.T) {
	for _, tc := range []struct {
		stack []string
		want  bool
	}{
		{[]string{"runtime.mallocgc", "runtime.newarray", "internal/runtime/maps.newarray", "internal/runtime/maps.(*table).grow", "internal/runtime/maps.(*Map).PutSlot", "runtime.mapassign", "main.f"}, true},
		{[]string{"runtime.mallocgc", "runtime.newobject", "type:.hash.main.key", "internal/runtime/maps.(*table).rehash", "runtime.mapassign_faststr", "main.f"}, true},
		{[]string{"runtime.mallocgc", "runtime.newobject", "type:.eq.main.key", "internal/runtime/maps.(*Map).Delete", "runtime.mapdelete", "main.f"}, true},
		{[]string{"runtime.mallocgc", "runtime.convTstring", "internal/runtime/maps.(*Map).Clear", "runtime.mapclear", "main.f"}, true},
		{[]string{"runtime.mallocgc", "runtime.typeAssert", "internal/runtime/maps.(*Map).Clear", "reflect.mapclear", "main.f"}, true},

		// The caller's own allocations, and map reads.
		{[]string{"runtime.mallocgc", "main.f", "runtime.mapassign"}, false},
		{[]string{"runtime.mallocgc", "runtime.convTstring", "main.f", "runtime.mapassign"}, false},
		{[]string{"runtime.mallocgc", "type:.hash.main.key", "main.f", "runtime.mapassign"}, false},
		{[]string{"runtime.mallocgc", "runtime.mapaccess1", "main.f"}, false},
		// A stack with no frame outside the runtime, and none at all.
		{[]string{"runtime.mallocgc", "runtime.gcBgMarkWorker"}, false},
		{nil, false},
	} {
		if got := runtime.GoplusInMapWrite(tc.stack...); got != tc.want {
			t.Errorf("goplusInMapWrite over %q = %v, want %v", tc.stack, got, tc.want)
		}
	}
}
