// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

var revisionRE = regexp.MustCompile(`^[0-9a-f]{40}(\+dirty)?$`)

// TestRelease checks the form of Release and Revision, and that Revision
// names a commit of the checkout the toolchain was built from.
func TestRelease(t *testing.T) {
	if r := goplus.Release(); !strings.HasPrefix(r, "v") {
		t.Errorf("Release() = %q, want a release tag such as v1.27.3b", r)
	}

	rev := goplus.Revision()
	if rev != "" && !revisionRE.MatchString(rev) {
		t.Fatalf("Revision() = %q, want a full commit hash, optionally followed by +dirty", rev)
	}

	goroot := testenv.GOROOT(t)
	if _, err := os.Stat(filepath.Join(goroot, ".git")); err != nil {
		if rev != "" {
			t.Logf("Revision() = %q, GOROOT %s is not a Git checkout; not checking the commit", rev, goroot)
		}
		return
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("GOROOT is a Git checkout but git is not available: %v", err)
	}
	if rev == "" {
		t.Fatalf("Revision() is empty, but the toolchain's GOROOT %s is a Git checkout", goroot)
	}
	sha := strings.TrimSuffix(rev, "+dirty")
	cmd := exec.Command(git, "cat-file", "-e", sha+"^{commit}")
	cmd.Dir = goroot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("Revision() = %q is not a commit of %s: %v\n%s", rev, goroot, err, out)
	}
}

// TestReleaseInBuildInfo checks that a program built with no build tags,
// and not importing goplus, can read the go-plus release and revision
// from its own build information (JIT level 13).
func TestReleaseInBuildInfo(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

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
		if s.Key == "goplus" || s.Key == "goplus.revision" {
			fmt.Println(s.Key + "=" + s.Value)
		}
	}
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
	want := []string{"goplus=" + goplus.Release()}
	if rev := goplus.Revision(); rev != "" {
		want = append(want, "goplus.revision="+rev)
	}
	if got := strings.TrimSpace(string(out)); got != strings.Join(want, "\n") {
		t.Errorf("program built with no tags read from its build info:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}

	// go version -m reads the same settings from outside the binary.
	vm := testenv.Command(t, testenv.GoToolPath(t), "version", "-m", exe)
	vout, err := vm.CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m: %v\n%s", err, vout)
	}
	for _, w := range want {
		if !strings.Contains(string(vout), "\tbuild\t"+w+"\n") {
			t.Errorf("go version -m does not show build %s:\n%s", w, vout)
		}
	}
}

// TestReleaseInOwnBuildInfo checks the release setting for this test
// binary, which go test builds: the setting is not limited to go build.
func TestReleaseInOwnBuildInfo(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("test binary has no build info (not built by the go command)")
	}
	for _, s := range info.Settings {
		if s.Key == "goplus" {
			if s.Value != goplus.Release() {
				t.Errorf("build setting goplus=%q, want %q", s.Value, goplus.Release())
			}
			return
		}
	}
	t.Errorf("test binary's build info has no goplus setting:\n%s", info)
}
