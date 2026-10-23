// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"go/ast"
	"go/parser"
	"go/token"
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
	"newcoro",        // the coroutine goroutine's gopc
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
	// goplusReturnAddress.
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
func callerPCAfterCall(body *ast.BlockStmt) (late, wrapped bool) {
	var calls []*ast.CallExpr
	wrappedAt := map[token.Pos]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
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
	wrapped = true
	for _, read := range calls {
		if callName(read) != "sys.GetCallerPC" {
			continue
		}
		for _, c := range calls {
			if c.End() <= read.Pos() && !harmlessCall(callName(c)) {
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
