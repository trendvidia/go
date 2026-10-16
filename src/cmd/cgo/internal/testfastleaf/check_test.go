// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf_test

import (
	"cmd/cgo/internal/testfastleaf"
	"context"
	"fmt"
	"internal/platform"
	"internal/race"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The GODEBUG=fastleafcheck tests run a child that sleeps in a fastleaf
// C function. A child that throws prints checkThrow; one that returns
// prints checkSlept. Each test asserts one of the two, so a child that
// did neither (failed to start, was killed) fails the test either way.
const (
	checkThrow = "fatal error: #cgo fastleaf call ran too long"
	checkSlept = "FASTLEAF_SLEPT"
)

// checkReport matches the line naming the call; the wrapper cgo
// generates names the C function.
var checkReport = regexp.MustCompile(`#cgo fastleaf call to \S*\._Cfunc_holdLeaf in goroutine \d+ has run for at least (\d+)ms, over GODEBUG=fastleafcheck=(\S+)`)

// sleepChild runs this test binary as a child that sleeps for ms
// milliseconds in a fastleaf call, with the given GODEBUG; with gc set,
// another goroutine starts a collection while it sleeps. It returns the
// child's output and how long it ran.
func sleepChild(t *testing.T, godebug string, ms int, gc bool) (string, time.Duration) {
	t.Helper()
	testenv.MustHaveExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := testenv.CommandContext(t, ctx, testenv.Executable(t), "-test.run=^TestFastLeafCheckChild$")
	cmd.Env = append(os.Environ(), "FASTLEAF_SLEEP_CHILD="+strconv.Itoa(ms), "GODEBUG="+godebug)
	if gc {
		cmd.Env = append(cmd.Env, "FASTLEAF_SLEEP_GC=1")
	}
	start := time.Now()
	out, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child did not finish within a minute:\n%s", out)
	}
	return string(out), time.Since(start)
}

// TestFastLeafCheckChild is the child of the tests below; it does nothing
// when run directly.
func TestFastLeafCheckChild(t *testing.T) {
	v := os.Getenv("FASTLEAF_SLEEP_CHILD")
	if v == "" {
		t.Skip("child of the fastleafcheck tests")
	}
	ms, err := strconv.Atoi(v)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FASTLEAF_SLEEP_GC") != "" {
		// The collection needs a P of its own while the sleeper holds one.
		runtime.GOMAXPROCS(4)
		go func() {
			time.Sleep(50 * time.Millisecond)
			runtime.GC()
		}()
	}
	testfastleaf.HoldLeaf(ms)
	fmt.Println(checkSlept)
}

func wantThrow(t *testing.T, out string, minMS int) {
	t.Helper()
	m := checkReport.FindStringSubmatch(out)
	if m == nil || !regexp.MustCompile(regexp.QuoteMeta(checkThrow)).MatchString(out) {
		t.Fatalf("child did not report the call as too long:\n%s", out)
	}
	if got, _ := strconv.Atoi(m[1]); got < minMS {
		t.Errorf("reported %sms, under the %s limit", m[1], m[2])
	}
	t.Log(m[0])
}

func wantSlept(t *testing.T, out string) {
	t.Helper()
	if !regexp.MustCompile(checkSlept).MatchString(out) || regexp.MustCompile(regexp.QuoteMeta(checkThrow)).MatchString(out) {
		t.Fatalf("child did not return from the call cleanly:\n%s", out)
	}
}

// TestFastLeafCheck checks that GODEBUG=fastleafcheck throws for a call
// over the limit, naming the C function, and not for one under it.
func TestFastLeafCheck(t *testing.T) {
	t.Run("over", func(t *testing.T) {
		out, took := sleepChild(t, "fastleafcheck=10ms", 5000, false)
		wantThrow(t, out, 10)
		// The throw came from sysmon during the call, not after it.
		if took >= 5*time.Second {
			t.Errorf("child ran %v, as long as the call", took)
		}
	})
	t.Run("under", func(t *testing.T) {
		out, _ := sleepChild(t, "fastleafcheck=2s", 100, false)
		wantSlept(t, out)
	})
	t.Run("off", func(t *testing.T) {
		if race.Enabled {
			t.Skip("the check is on by default under the race detector")
		}
		out, _ := sleepChild(t, "", 300, false)
		wantSlept(t, out)
	})
}

// TestFastLeafCheckStopTheWorld checks the case the check exists for: a
// long call holding up a stop-the-world. sysmon sleeps until the next
// timer while the world is stopping, unless the check is on.
func TestFastLeafCheckStopTheWorld(t *testing.T) {
	out, took := sleepChild(t, "fastleafcheck=500ms", 5000, true)
	wantThrow(t, out, 500)
	if took >= 5*time.Second {
		t.Errorf("child ran %v, as long as the call", took)
	}
}

// TestFastLeafCheckRace checks that the race detector turns the check on
// with a 1s limit, and that fastleafcheck=0 turns it off again.
func TestFastLeafCheckRace(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("no race detector on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "sleepleaf", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sleepleaf\n\ngo 1.27\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "sleepleaf"+exeSuffix())
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-race", "-o", exe, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -race: %v\n%s", err, out)
	}

	run := func(godebug string, ms int) string {
		cmd := exec.Command(exe, strconv.Itoa(ms))
		cmd.Env = append(os.Environ(), "GODEBUG="+godebug)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := run("", 5000); !regexp.MustCompile(`has run for at least \d+ms, over GODEBUG=fastleafcheck=1000ms`).MatchString(out) || !regexp.MustCompile(regexp.QuoteMeta(checkThrow)).MatchString(out) {
		t.Errorf("under -race, a 5s call was not reported against a 1s limit:\n%s", out)
	}
	if out := run("fastleafcheck=0", 1500); !regexp.MustCompile(`(?m)^slept$`).MatchString(out) {
		t.Errorf("under -race with fastleafcheck=0, a 1.5s call did not return cleanly:\n%s", out)
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
