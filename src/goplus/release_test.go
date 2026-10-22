// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"internal/goversion"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestReleaseInBuildInfo checks that a program built with no build tags,
// and not importing goplus, can read the go-plus release from its own
// build information (JIT level 13).
func TestReleaseInBuildInfo(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	if !strings.HasPrefix(goversion.PlusRelease, "v") {
		t.Fatalf("goversion.PlusRelease = %q, want a release tag such as v1.27.3b", goversion.PlusRelease)
	}

	dir := t.TempDir()
	const prog = `package main

import (
	"fmt"
	"runtime/debug"
)

func main() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("no build info")
		return
	}
	for _, s := range info.Settings {
		if s.Key == "goplus" {
			fmt.Println("goplus=" + s.Value)
			return
		}
	}
	fmt.Println("no goplus setting")
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module release\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "release.exe")
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(exe).CombinedOutput()
	if err != nil {
		t.Fatalf("running the built program: %v\n%s", err, out)
	}
	want := "goplus=" + goversion.PlusRelease
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("program built with no tags read %q from its build info, want %q", got, want)
	}

	// go version -m reads the same setting from outside the binary.
	vm := testenv.Command(t, testenv.GoToolPath(t), "version", "-m", exe)
	vout, err := vm.CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m: %v\n%s", err, vout)
	}
	if !strings.Contains(string(vout), "\tbuild\t"+want+"\n") {
		t.Errorf("go version -m does not show build %s:\n%s", want, vout)
	}
}

// TestReleaseInOwnBuildInfo checks the same for this test binary, which
// go test builds: the setting is not limited to go build.
func TestReleaseInOwnBuildInfo(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("test binary has no build info (not built by the go command)")
	}
	for _, s := range info.Settings {
		if s.Key == "goplus" {
			if s.Value != goversion.PlusRelease {
				t.Errorf("build setting goplus=%q, want %q", s.Value, goversion.PlusRelease)
			}
			return
		}
	}
	t.Errorf("test binary's build info has no goplus setting:\n%s", info)
}
