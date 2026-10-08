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

// The reproduction from trendvidia/go#158. Inlining Desc("A") and
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
// "A" twice (trendvidia/go#158).
//
// The slots are taken under -shared, which cmd/go passes for PIE on
// darwin and linux. That is every default darwin build.
func TestInlinedClosureFuncPtrSlots(t *testing.T) {
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
	exe := filepath.Join(dir, "repro.exe")
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-buildmode=pie", "-o", exe, ".")
	build.Dir = dir
	build.Env = append(build.Environ(), "GOFLAGS=", "GO111MODULE=on")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v:\n%s", build.Args, err, out)
	}

	// The test only means something if the build took the slot path.
	// Look for any go.funcptr slot in the binary, not for one of
	// Desc.func1's: the fix is that Desc.func1 no longer has one.
	nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	syms, err := nm.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v:\n%s", nm.Args, err, syms)
	}
	if !strings.Contains(string(syms), " go.funcptr.") {
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
