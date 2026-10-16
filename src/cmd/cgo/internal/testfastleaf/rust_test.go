// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFastLeafRust builds testdata/rustleaf: a Rust function exported
// with extern "C", linked as a static library and called from Go through
// #cgo fastleaf. The program checks the results across collections and
// that a Rust panic is caught at the boundary.
func TestFastLeafRust(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	rustc, err := exec.LookPath("rustc")
	if err != nil {
		t.Skip("rustc not found")
	}
	host := rustHost(t, rustc)
	if runtime.GOOS == "windows" && !strings.Contains(host, "-gnu") {
		t.Skipf("rustc's host target is %s; cgo on windows links with mingw, which needs a -gnu Rust target", host)
	}

	dir := t.TempDir()
	for _, f := range []string{"main.go", "rustleaf.rs"} {
		b, err := os.ReadFile(filepath.Join("testdata", "rustleaf", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module rustleaf\n\ngo 1.27\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	lib := filepath.Join(dir, "librustleaf.a")
	cmd := exec.Command(rustc, "--crate-type=staticlib", "--edition=2021", "-O",
		"--print", "native-static-libs", "-o", lib, "rustleaf.rs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rustc: %v\n%s", err, out)
	}
	native := ""
	for _, line := range strings.Split(string(out), "\n") {
		if _, libs, ok := strings.Cut(line, "native-static-libs:"); ok {
			native = strings.TrimSpace(libs)
		}
	}

	exe := filepath.Join(dir, "rustleaf.exe")
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_LDFLAGS="+lib+" "+native)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := testenv.Command(t, exe)
	out, err = run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK:") {
		t.Fatalf("rustleaf: %v\n%s", err, out)
	}
	t.Logf("%s (rustc host %s)", strings.TrimSpace(lastLine(string(out))), host)
}

func rustHost(t *testing.T, rustc string) string {
	out, err := exec.Command(rustc, "-vV").Output()
	if err != nil {
		t.Fatalf("rustc -vV: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(line, "host: "); ok {
			return strings.TrimSpace(h)
		}
	}
	return ""
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
