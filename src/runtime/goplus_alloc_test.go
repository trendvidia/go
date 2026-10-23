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
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !setsWriting(fd.Body) {
				continue
			}
			writers = append(writers, mapsSymbol(fd))
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

// setsWriting reports whether body toggles a map's writing mark, as in
// m.writing ^= 1.
func setsWriting(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.XOR_ASSIGN || len(as.Lhs) != 1 {
			return !found
		}
		if sel, ok := as.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "writing" {
			found = true
		}
		return !found
	})
	return found
}

// mapsSymbol returns the symbol name of fd, a function of package
// internal/runtime/maps: the target of its linkname directive, if it has
// one, or the name the compiler gives it.
func mapsSymbol(fd *ast.FuncDecl) string {
	if fd.Doc != nil {
		for _, c := range fd.Doc.List {
			fields := strings.Fields(c.Text)
			if len(fields) == 3 && fields[0] == "//go:linkname" && fields[1] == fd.Name.Name {
				return fields[2]
			}
		}
	}
	const pkg = "internal/runtime/maps."
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return pkg + fd.Name.Name
	}
	switch recv := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		return pkg + "(*" + recv.X.(*ast.Ident).Name + ")." + fd.Name.Name
	case *ast.Ident:
		return pkg + recv.Name + "." + fd.Name.Name
	}
	panic("unexpected receiver of " + fd.Name.Name)
}
