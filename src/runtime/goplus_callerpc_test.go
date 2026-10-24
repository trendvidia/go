// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// goplusCallerPCKept lists the runtime functions a stack-limit refusal may
// wait for (goplusLateRefusable) that read their own return address after
// a call that can grow the stack, and keep it for a use the unwinder does
// not read it through. While a refusal is pending on their return, that
// address is goplusHijackReturn, so each passes it through
// goplusReturnAddress, which TestGoplusCallerPCReaders checks.
var goplusCallerPCKept = []string{
	"deferproc",      // d.pc, where recovery resumes (findfunc)
	"deferrangefunc", // d.pc, likewise
}

// TestGoplusLinknamedStd checks that goplusLinknamedStd lists exactly the
// runtime functions that //go:linknamestd lets other packages of the
// standard library call by name, which a stack-limit refusal must not
// wait for. A new one, as an upstream change or the fork can add, fails
// here until it is listed.
func TestGoplusLinknamedStd(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if fields := strings.Fields(c.Text); len(fields) == 2 && fields[0] == "//go:linknamestd" {
					found = append(found, fields[1])
				}
			}
		}
	}
	found = slices.Compact(slices.Sorted(slices.Values(found)))
	if len(found) == 0 {
		t.Fatal("found no //go:linknamestd directive in the runtime; the test does not test that")
	}
	if want := slices.Sorted(slices.Values(runtime.GoplusLinknamedStd)); !slices.Equal(found, want) {
		t.Errorf("//go:linknamestd functions %v, goplusLinknamedStd %v", found, want)
	}
	for _, name := range found {
		if runtime.GoplusLateRefusable("runtime." + name) {
			t.Errorf("goplusLateRefusable(runtime.%s) = true, want false", name)
		}
	}
}

// goplusCallerPCOther lists the other such readers, and why the address
// they read may be goplusHijackReturn's.
var goplusCallerPCOther = map[string]string{
	// Called only by the runtime, never by code that expects a panic: a
	// refusal never overwrites their return address (goplusPanicSafeAt).
	"copyBlockProfileRecord":               "runtime callers", // BlockProfile, MutexProfile
	"copyMemProfileRecord":                 "runtime callers", // MemProfile
	"entersyscallblock":                    "runtime callers", // notetsleepg, sigNoteSleep
	"goroutineProfileWithLabelsConcurrent": "runtime callers", // goroutineProfileWithLabels
	"goroutineProfileWithLabelsSync":       "runtime callers", // goroutineProfileWithLabels
	"mstart1":                              "runtime callers", // on the system stack
	// Read for the race detector, which reports it as the place of an
	// access: a report while a refusal is pending names the trampoline.
	"closechan":         "race",
	"makeslicecopy":     "race",
	"slicerunetostring": "race",
	// Read into gp.syscallpc or m.libcallpc, where tracebacks and the CPU
	// profiler start; the unwinder's initAt passes it through
	// goplusReturnAddress. The execution tracer reads gp.syscallpc raw,
	// which is safe because each of these is nosplit and reads it before
	// any call that could grow the stack, so no refusal can be pending on
	// its frame yet.
	"cgocallLeaf": "initAt",
	"libcCall":    "initAt",
	"stdcall":     "initAt",
	// The walk stops at a frame with a special funcID (FuncID_gopanic).
	"gopanic": "funcID",
	// Read into m.libcallpc, where the CPU profiler starts a traceback;
	// the unwinder's initAt passes it through goplusReturnAddress.
	"syscall0": "initAt", "syscall1": "initAt", "syscall2": "initAt", "syscall3": "initAt",
	"syscall4": "initAt", "syscall5": "initAt", "syscall6": "initAt",
	"sysvicall0": "initAt", "sysvicall1Err": "initAt", "sysvicall2Err": "initAt",
	"sysvicall3Err": "initAt", "sysvicall4": "initAt", "sysvicall5": "initAt",
	"sysvicall6": "initAt",
}

// TestGoplusCallerPCReaders pins the audit behind goplusReturnAddress: of
// the runtime functions a refusal may wait for, every one that reads
// sys.GetCallerPC after another call either passes it through
// goplusReturnAddress or is listed above with a reason. A new reader, as
// an upstream change can bring, fails here until someone decides which.
func TestGoplusCallerPCReaders(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var translated, other []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		pushed := linknamePushed(f)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || pushed[fn.Name.Name] {
				continue
			}
			if !runtime.GoplusLateRefusable("runtime." + fn.Name.Name) {
				continue
			}
			late, wrapped := callerPCAfterCall(fn.Body)
			switch {
			case !late:
			case wrapped:
				translated = append(translated, fn.Name.Name)
			default:
				other = append(other, fn.Name.Name)
			}
		}
	}
	slices.Sort(translated)
	translated = slices.Compact(translated)
	want := slices.Sorted(slices.Values(goplusCallerPCKept))
	if !slices.Equal(translated, want) {
		t.Errorf("functions passing a late GetCallerPC through goplusReturnAddress: %v, want %v", translated, want)
	}
	for _, name := range slices.Compact(slices.Sorted(slices.Values(other))) {
		if _, ok := goplusCallerPCOther[name]; !ok {
			t.Errorf("runtime.%s reads sys.GetCallerPC after a call that can grow the stack, and a stack-limit refusal may wait for it to return: pass the address through goplusReturnAddress, or list it in goplusCallerPCOther with why it need not be", name)
		}
	}
	for name := range goplusCallerPCOther {
		if !slices.Contains(other, name) {
			t.Errorf("goplusCallerPCOther lists runtime.%s, which no longer reads sys.GetCallerPC after a call", name)
		}
	}
}

// linknamePushed returns the functions of f that a //go:linkname directive
// gives another package's name, which a refusal never waits for.
func linknamePushed(f *ast.File) map[string]bool {
	pushed := map[string]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			fields := strings.Fields(c.Text)
			if len(fields) == 3 && fields[0] == "//go:linkname" && !strings.HasPrefix(fields[2], "runtime.") {
				pushed[fields[1]] = true
			}
		}
	}
	return pushed
}

// callerPCAfterCall reports whether body calls sys.GetCallerPC after a
// call that is not one of a few that cannot grow the stack or never
// return, and whether every such read is passed to goplusReturnAddress.
// After means earlier in the source, or anywhere in a loop, or between a
// label and a goto back to it, that holds the read too: the read runs
// again after the call.
func callerPCAfterCall(body *ast.BlockStmt) (late, wrapped bool) {
	var calls []*ast.CallExpr
	wrappedAt := map[token.Pos]bool{}
	// The parts of body that can run more than once: loops, and the
	// statements from a label to a goto after it.
	type region struct{ from, to token.Pos }
	var repeats []region
	labels := map[string]token.Pos{}
	var gotos []*ast.BranchStmt
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ForStmt, *ast.RangeStmt:
			repeats = append(repeats, region{n.Pos(), n.End()})
		case *ast.LabeledStmt:
			labels[n.Label.Name] = n.Pos()
		case *ast.BranchStmt:
			if n.Tok == token.GOTO {
				gotos = append(gotos, n)
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calls = append(calls, call)
		if callName(call) == "goplusReturnAddress" {
			for _, arg := range call.Args {
				if a, ok := arg.(*ast.CallExpr); ok && callName(a) == "sys.GetCallerPC" {
					wrappedAt[a.Pos()] = true
				}
			}
		}
		return true
	})
	for _, g := range gotos {
		if at, ok := labels[g.Label.Name]; ok && at < g.Pos() {
			repeats = append(repeats, region{at, g.End()})
		}
	}
	// repeated reports whether a and b are both in one part that repeats.
	repeated := func(a, b ast.Node) bool {
		for _, r := range repeats {
			if r.from <= a.Pos() && a.End() <= r.to && r.from <= b.Pos() && b.End() <= r.to {
				return true
			}
		}
		return false
	}
	wrapped = true
	for _, read := range calls {
		if callName(read) != "sys.GetCallerPC" {
			continue
		}
		for _, c := range calls {
			if (c.End() <= read.Pos() || repeated(c, read)) && c != read && !harmlessCall(callName(c)) {
				late = true
				wrapped = wrapped && wrappedAt[read.Pos()]
				break
			}
		}
	}
	return late, late && wrapped
}

func callName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			return x.Name + "." + fun.Sel.Name
		}
	}
	return ""
}

// harmlessCall reports whether a call to name cannot move the stack under
// a later GetCallerPC: it does not return, or cannot grow the stack, or
// is a conversion.
func harmlessCall(name string) bool {
	if strings.HasPrefix(name, "panic") {
		return true // panicunsafeslicelen1 and the like
	}
	switch name {
	case "throw", "fatal", "getg", "sys.GetCallerPC", "sys.GetCallerSP", "getcallerfp",
		"len", "cap", "add", "noescape", "math.MulUintptr",
		// Conversions.
		"unsafe.Pointer", "uintptr", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune":
		return true
	}
	return false
}

// TestGoplusCallerPCAfterCall checks the audit TestGoplusCallerPCReaders
// runs on each function, on bodies written to be caught or let pass.
func TestGoplusCallerPCAfterCall(t *testing.T) {
	for _, tc := range []struct {
		body          string
		late, wrapped bool
	}{
		{"pc := sys.GetCallerPC(); f(); _ = pc", false, false},
		{"f(); pc := sys.GetCallerPC(); _ = pc", true, false},
		{"f(); pc := goplusReturnAddress(gp, sys.GetCallerPC()); _ = pc", true, true},
		{"throw(\"x\"); pc := sys.GetCallerPC(); _ = pc", false, false},
		// The read runs again after the call.
		{"for { pc := sys.GetCallerPC(); f(); _ = pc }", true, false},
		{"for range n { pc := sys.GetCallerPC(); _ = pc; f() }", true, false},
		{"again: pc := sys.GetCallerPC(); if f() { goto again }; _ = pc", true, false},
		// A loop, or a goto forward, that holds only one of the two.
		{"for { f() }; pc := sys.GetCallerPC(); _ = pc", true, false},
		{"pc := sys.GetCallerPC(); for { f() }; _ = pc", false, false},
		{"pc := sys.GetCallerPC(); goto out; out: f(); _ = pc", false, false},
		// A closure reads its own caller.
		{"g := func() { f(); _ = sys.GetCallerPC() }; _ = g", false, false},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\nfunc x() {\n"+tc.body+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		body := f.Decls[0].(*ast.FuncDecl).Body
		if late, wrapped := callerPCAfterCall(body); late != tc.late || wrapped != tc.wrapped {
			t.Errorf("callerPCAfterCall(%s) = %v, %v; want %v, %v", tc.body, late, wrapped, tc.late, tc.wrapped)
		}
	}
}

// TestGoplusPullCoroLast checks that iter.Pull and iter.Pull2 make their
// coroutine last, after the next and stop functions they return: the
// coroutine is ended only through those, so a refusal by the stack limit
// or the allocation budget between newcoro and the return would leave it
// parked forever. Allocating next's and stop's closures after newcoro, as
// upstream does, is such a place.
func TestGoplusPullCoroLast(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "iter", "iter.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != "Pull" && fn.Name.Name != "Pull2" {
			continue
		}
		checked++
		at := -1
		assigned := map[string]bool{}
		for i, stmt := range fn.Body.List {
			if at < 0 && callsNamed(stmt, "newcoro") {
				at = i
				continue
			}
			if as, ok := stmt.(*ast.AssignStmt); ok && at < 0 {
				for _, lhs := range as.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						assigned[id.Name] = true
					}
				}
			}
		}
		if at < 0 {
			t.Errorf("%s: no call to newcoro", fn.Name.Name)
			continue
		}
		if !assigned["next"] || !assigned["stop"] {
			t.Errorf("%s: next and stop are not both assigned before newcoro", fn.Name.Name)
		}
		rest := fn.Body.List[at+1:]
		if len(rest) != 1 {
			t.Errorf("%s: %d statements follow newcoro, want only the return", fn.Name.Name, len(rest))
			continue
		}
		ret, ok := rest[0].(*ast.ReturnStmt)
		if !ok {
			t.Errorf("%s: newcoro is followed by %T, want the return", fn.Name.Name, rest[0])
			continue
		}
		for _, r := range ret.Results {
			if _, ok := r.(*ast.Ident); !ok {
				t.Errorf("%s: the return after newcoro computes a result", fn.Name.Name)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("checked %d of iter.Pull and iter.Pull2; the test does not test them", checked)
	}
}

// callsNamed reports whether n contains a call to a function named name.
func callsNamed(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestGoplusLinknamePushNotOrdinary checks that no function the runtime
// defines under another package's name, by a push //go:linkname, counts
// as ordinary code for the stack limit's callee rule (trendvidia/go#105).
// Their names say another package, maps.clone or reflect.typedmemmove,
// but they are runtime code: a refusal at their entry would raise a panic
// in the runtime's frames if the runtime called one. Each such function
// linked into this binary is checked.
func TestGoplusLinknamePushNotOrdinary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var pushed []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Only files built here: a directive in sys_openbsd.go names
		// syscall.syscall, which on other systems is package syscall's
		// own function.
		if ok, err := build.Default.MatchFile(".", name); err != nil {
			t.Fatal(err)
		} else if !ok {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		defined := map[string]bool{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Recv == nil {
				defined[fn.Name.Name] = true
			}
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				fields := strings.Fields(c.Text)
				if len(fields) == 3 && fields[0] == "//go:linkname" && defined[fields[1]] && !strings.HasPrefix(fields[2], "runtime.") {
					pushed = append(pushed, fields[2])
				}
			}
		}
	}
	pushed = slices.Compact(slices.Sorted(slices.Values(pushed)))
	// maps.Clone calls maps.clone, which is runtime.mapclone: this keeps
	// one such function in the binary whatever else the tests use.
	_ = maps.Clone(map[int]int{1: 1})
	var linked []string
	for _, name := range pushed {
		found, ordinary := runtime.GoplusOrdinaryFunc(name)
		if !found {
			continue
		}
		linked = append(linked, name)
		if ordinary {
			t.Errorf("%s is runtime code under another package's name, and counts as ordinary code", name)
		}
	}
	// Positive marker: maps.clone at least is linked in, by the reference
	// above.
	if !slices.Contains(linked, "maps.clone") {
		t.Fatalf("maps.clone is not among the %d push-linknamed runtime functions found in the binary (%d in the source); the test does not test that", len(linked), len(pushed))
	}
	t.Logf("%d push-linknamed runtime functions in the source, %d in this binary", len(pushed), len(linked))
}
