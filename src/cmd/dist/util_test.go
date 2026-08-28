// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFatalfInExitHandler checks that a fatalf raised while xexit is
// running the atexit handlers kills the process after printing the
// message once, instead of re-entering xexit and re-running the
// failing handler forever (#92). The handlers that have not run yet
// still run: the work directory cleanup is registered first, so it is
// the last one to run and the one a bare os.Exit would drop.
func TestFatalfInExitHandler(t *testing.T) {
	if os.Getenv("DIST_TEST_FATALF_IN_EXIT_HANDLER") == "1" {
		// Handlers run in reverse registration order, so this runs last.
		xatexit(func() { xprintf("cleanup handler ran\n") })
		xatexit(func() { fatalf("first exit handler failed") })
		xatexit(func() { fatalf("second exit handler failed") })
		fatalf("original failure")
		panic("unreachable")
	}

	cmd := exec.Command(testenv.Executable(t), "-test.run=^TestFatalfInExitHandler$")
	cmd.Env = append(os.Environ(), "DIST_TEST_FATALF_IN_EXIT_HANDLER=1")
	out, err := cmd.CombinedOutput()

	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		t.Errorf("subprocess: got %v, want exit status 2", err)
	}
	for _, msg := range []string{
		"go tool dist: original failure",
		"go tool dist: second exit handler failed",
		"go tool dist: first exit handler failed",
		"cleanup handler ran",
	} {
		if n := strings.Count(string(out), msg); n != 1 {
			t.Errorf("output contains %q %d times, want exactly once", msg, n)
		}
	}
	if t.Failed() {
		t.Logf("subprocess output:\n%s", out)
	}
}

// TestRemoveIfEmpty checks the cleanup that setup registers for
// pkg/GOOS_GOARCH: it removes the directory only when it is empty, and a
// directory that a concurrent make.bash already removed is not an error.
// Reading it with xreaddir instead called fatalf, which killed the process
// from inside an exit handler (#92).
func TestRemoveIfEmpty(t *testing.T) {
	dir := t.TempDir()

	// Missing: nothing to do, and no fatalf. A fatalf here exits the
	// test binary with status 2 rather than failing this test.
	xremoveIfEmpty(filepath.Join(dir, "gone"))

	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o777); err != nil {
		t.Fatal(err)
	}
	xremoveIfEmpty(empty)
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Errorf("empty dir: got stat error %v, want IsNotExist", err)
	}

	full := filepath.Join(dir, "full")
	if err := os.Mkdir(full, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "f"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	xremoveIfEmpty(full)
	if _, err := os.Stat(full); err != nil {
		t.Errorf("non-empty dir: got stat error %v, want it kept", err)
	}
}
