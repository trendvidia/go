// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The reproduction from trendvidia/go-archive#158. Inlining Desc("A") and
// Desc("B") into main makes two copies of Desc's closure, each
// specialised on its constant capture. Inside the compiler they are
// told apart by an inline hash (lib.Desc.func1#<hash>#), which the
// object writer strips, so both reach the linker as lib.Desc.func1.
var inlClosureSrc = map[string]string{
	"go.mod": "module repro\n\ngo 1.27\n",
	"lib/lib.go": `package lib

type Opt func(m map[string]any)

func Desc(d string) Opt { return func(m map[string]any) { m["d"] = d } }

type Tool struct{ Props map[string]any }
type ToolOpt func(*Tool)

func WithString(name string, opts ...Opt) ToolOpt {
	return func(t *Tool) {
		m := map[string]any{"type": "string"}
		for _, o := range opts {
			o(m)
		}
		t.Props[name] = m
	}
}
`,
	"main.go": `package main

import (
	"fmt"

	"repro/lib"
)

func main() {
	t := &lib.Tool{Props: map[string]any{}}
	oa := lib.WithString("a", lib.Desc("A"))
	ob := lib.WithString("b", lib.Desc("B"))
	oa(t)
	ob(t)
	fmt.Println(t.Props["a"], t.Props["b"])
}
`,
}

// TestInlinedClosureFuncPtrSlots checks that copies of a closure made by
// inlining keep their own bodies when function values are loaded through
// go.funcptr slots. A slot was named after the closure, and the closure's
// name loses its inline hash in the object file, so all copies shared
// one DUPOK slot and called the first copy's body: the program printed
// "A" twice (trendvidia/go-archive#158).
//
// The slots are taken under -shared, which cmd/go passes for PIE on
// darwin and linux. That is every default darwin build.
func TestInlinedClosureFuncPtrSlots(t *testing.T) {
	exe, syms := buildInlClosurePIE(t)

	// The test only means something if the build took the slot path.
	// Look for any go.funcptr slot in the binary, not for one of
	// Desc.func1's: the fix is that Desc.func1 no longer has one.
	if !strings.Contains(syms, " go.funcptr.") {
		t.Skipf("%s/%s pie build has no go.funcptr slots, so it does not take the path this test checks", runtime.GOOS, runtime.GOARCH)
	}

	out, err := testenv.Command(t, exe).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v:\n%s", exe, err, out)
	}
	const want = "map[d:A type:string] map[d:B type:string]\n"
	if string(out) != want {
		t.Errorf("program printed %q, want %q", out, want)
	}
}

// TestPIEBuildsTakeFuncPtrSlots pins the decision in trendvidia/go-archive#161:
// the compiler takes the go.funcptr slot path on -shared, so PIE builds
// on darwin and linux get slots even when they never load a plugin.
// Restricting the slots to -dynlink was measured and declined, to keep
// what PIE hosts do at load unchanged. If this fails, the slots were
// restricted; that reverses the decision, so revisit trendvidia/go-archive#161 rather than
// deleting this test.
func TestPIEBuildsTakeFuncPtrSlots(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("the decision covers darwin and linux; %s PIE builds do not take the slot path", runtime.GOOS)
	}
	_, syms := buildInlClosurePIE(t)
	if !strings.Contains(syms, " go.funcptr.") {
		t.Errorf("%s/%s -buildmode=pie build has no go.funcptr slots; trendvidia/go-archive#161 decided PIE builds keep them", runtime.GOOS, runtime.GOARCH)
	}
}

// buildInlClosurePIE builds inlClosureSrc with -buildmode=pie and returns
// the executable's path and its go tool nm output.
func buildInlClosurePIE(t *testing.T) (exe, syms string) {
	t.Helper()
	testenv.MustHaveGoRun(t)
	if !platform.BuildModeSupported("gc", "pie", runtime.GOOS, runtime.GOARCH) {
		t.Skipf("-buildmode=pie is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	t.Parallel()

	dir := t.TempDir()
	for name, src := range inlClosureSrc {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exe = filepath.Join(dir, "repro.exe")
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-buildmode=pie", "-o", exe, ".")
	build.Dir = dir
	build.Env = append(build.Environ(), "GOFLAGS=", "GO111MODULE=on")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v:\n%s", build.Args, err, out)
	}
	nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	out, err := nm.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v:\n%s", nm.Args, err, out)
	}
	return exe, string(out)
}
