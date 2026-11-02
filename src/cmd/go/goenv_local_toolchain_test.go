// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"internal/testenv"
)

// TestGoEnvKeepsFork checks that this fork's go.env refuses to switch
// toolchains. A module asking for a newer Go must fail, because the toolchain
// auto would fetch instead is stock Go, and a build on it succeeds without the
// fork's plugin and goplus support (trendvidia/go#115).
//
// It runs the go command rather than reading go.env, and clears GOTOOLCHAIN
// from the environment, since script tests set it to auto themselves.
func TestGoEnvKeepsFork(t *testing.T) {
	testenv.MustHaveExec(t)

	dir := t.TempDir()
	const gomod = "module example.com/keepsfork\n\ngo 1.999\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o666); err != nil {
		t.Fatal(err)
	}

	cmd := testenv.Command(t, testenv.GoToolPath(t), "list", "-m")
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOTOOLCHAIN=") || strings.HasPrefix(kv, "GOENV=")
	})
	cmd.Env = append(cmd.Env, "GOENV=off")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("go list succeeded in a module requiring go 1.999:\n%s", out)
	}
	if !strings.Contains(string(out), "GOTOOLCHAIN=local") {
		t.Fatalf("go list did not refuse with GOTOOLCHAIN=local:\n%s", out)
	}
}
