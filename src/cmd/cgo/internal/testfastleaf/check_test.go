// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf_test

import (
	"bufio"
	"cmd/cgo/internal/testfastleaf"
	"context"
	"fmt"
	"internal/platform"
	"internal/race"
	"internal/testenv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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
	checkReady = "FASTLEAF_READY"
)

// checkReport matches the line naming the call; the wrapper cgo
// generates names the C function.
var checkReport = regexp.MustCompile(`#cgo fastleaf call to \S*\._Cfunc_holdLeaf in goroutine \d+ has run for at least (\d+)(ns|us|ms), over GODEBUG=fastleafcheck=((\d+)(ns|us|ms))\n`)

// childCommand returns a command that runs exe, this test binary or a
// build of it, as a child that sleeps for ms milliseconds in fastleaf
// calls, with the given GODEBUG. mode is how it sleeps: "" in one call;
// "gc" in one call while another goroutine starts a collection; "many"
// in back-to-back 1ms calls; "wake" in one call, started by a goroutine
// the network poller wakes when the parent writes to its stdin.
func childCommand(t *testing.T, ctx context.Context, exe, godebug string, ms int, mode string) *exec.Cmd {
	t.Helper()
	testenv.MustHaveExec(t)
	cmd := testenv.CommandContext(t, ctx, exe, "-test.run=^TestFastLeafCheckChild$")
	cmd.Env = append(os.Environ(), "FASTLEAF_SLEEP_CHILD="+strconv.Itoa(ms), "FASTLEAF_SLEEP_MODE="+mode, "GODEBUG="+godebug)
	return cmd
}

// sleepChild runs a child (see childCommand) and returns its output and
// how long it ran.
func sleepChild(t *testing.T, exe, godebug string, ms int, mode string) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := childCommand(t, ctx, exe, godebug, ms, mode)
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
	switch mode := os.Getenv("FASTLEAF_SLEEP_MODE"); mode {
	case "":
		testfastleaf.HoldLeaf(ms)
	case "gc":
		// The collection needs a P of its own while the sleeper holds one.
		runtime.GOMAXPROCS(4)
		go func() {
			time.Sleep(50 * time.Millisecond)
			runtime.GC()
		}()
		testfastleaf.HoldLeaf(ms)
	case "many":
		// Many short calls on one M, each under the limit, together
		// far over it.
		for start := time.Now(); time.Since(start) < time.Duration(ms)*time.Millisecond; {
			testfastleaf.HoldLeaf(1)
		}
	case "wake":
		waitForStdin(t)
		testfastleaf.HoldLeaf(ms)
	default:
		t.Fatalf("unknown FASTLEAF_SLEEP_MODE %q", mode)
	}
	fmt.Println(checkSlept)
}

// wantThrow checks that out reports a call over limit, a duration as the
// report prints it, and no shorter than it.
func wantThrow(t *testing.T, out, limit string) {
	t.Helper()
	m := checkReport.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, checkThrow) {
		t.Fatalf("child did not report the call as too long:\n%s", out)
	}
	if m[3] != limit {
		t.Errorf("report names the limit as %s, want %s", m[3], limit)
	}
	ran, _ := strconv.ParseInt(m[1], 10, 64)
	lim, _ := strconv.ParseInt(m[4], 10, 64)
	if m[2] != m[5] || ran < lim {
		t.Errorf("reported %s%s, under the %s limit", m[1], m[2], m[3])
	}
	t.Log(strings.TrimSpace(m[0]))
}

func wantSlept(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, checkSlept) || strings.Contains(out, checkThrow) {
		t.Fatalf("child did not return from the call cleanly:\n%s", out)
	}
}

// TestFastLeafCheck checks that GODEBUG=fastleafcheck throws for a call
// over the limit, naming the C function, and not for one under it.
func TestFastLeafCheck(t *testing.T) {
	exe := testenv.Executable(t)
	t.Run("over", func(t *testing.T) {
		out, took := sleepChild(t, exe, "fastleafcheck=10ms", 5000, "")
		wantThrow(t, out, "10ms")
		// The throw came from sysmon during the call, not after it.
		if took >= 5*time.Second {
			t.Errorf("child ran %v, as long as the call", took)
		}
	})
	t.Run("unit", func(t *testing.T) {
		// A limit that is not a whole number of milliseconds is reported
		// as it was set, not rounded down.
		out, _ := sleepChild(t, exe, "fastleafcheck=15500us", 5000, "")
		wantThrow(t, out, "15500us")
	})
	t.Run("under", func(t *testing.T) {
		out, _ := sleepChild(t, exe, "fastleafcheck=2s", 100, "")
		wantSlept(t, out)
	})
	t.Run("many", func(t *testing.T) {
		// Calls are told apart by the M's call counter, not by the flag
		// alone, which is set nearly all the time here.
		out, _ := sleepChild(t, exe, "fastleafcheck=50ms", 500, "many")
		wantSlept(t, out)
	})
	t.Run("off", func(t *testing.T) {
		if race.Enabled {
			t.Skip("the check is on by default under the race detector")
		}
		out, _ := sleepChild(t, exe, "", 300, "")
		wantSlept(t, out)
	})
}

// TestFastLeafCheckStopTheWorld checks the case the check exists for: a
// long call holding up a stop-the-world. sysmon sleeps until the next
// timer while the world is stopping, unless the check is on.
func TestFastLeafCheckStopTheWorld(t *testing.T) {
	out, took := sleepChild(t, testenv.Executable(t), "fastleafcheck=500ms", 5000, "gc")
	wantThrow(t, out, "500ms")
	if took >= 5*time.Second {
		t.Errorf("child ran %v, as long as the call", took)
	}
}

// TestFastLeafCheckIdle checks a call that starts while sysmon sleeps
// because every P is idle. Nothing in a fastleaf call wakes it, and a
// goroutine the network poller wakes starts running without waking it
// either, so with the check on sysmon must not sleep past the limit.
func TestFastLeafCheckIdle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the child waits on its stdin through the network poller, which needs a Unix pipe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := childCommand(t, ctx, testenv.Executable(t), "fastleafcheck=100ms", 5000, "wake")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var out strings.Builder
	br := bufio.NewReader(r)
	for !strings.Contains(out.String(), checkReady) {
		line, err := br.ReadString('\n')
		out.WriteString(line)
		if err != nil {
			cmd.Wait()
			t.Fatalf("child did not get ready:\n%s", out.String())
		}
	}
	// Long enough for the child to go idle and sysmon to sleep.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	stdin.Write([]byte{0})
	rest, _ := io.ReadAll(br)
	took := time.Since(start)
	cmd.Wait()
	out.Write(rest)
	if ctx.Err() != nil {
		t.Fatalf("child did not finish within a minute:\n%s", out.String())
	}
	wantThrow(t, out.String(), "100ms")
	if took >= 5*time.Second {
		t.Errorf("child ran %v after waking, as long as the call", took)
	}
}

// TestFastLeafCheckRace checks that the race detector turns the check on
// with a 1s limit, and that fastleafcheck=0 turns it off again.
func TestFastLeafCheckRace(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("no race detector on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe := filepath.Join(t.TempDir(), "testfastleaf.race"+exeSuffix())
	build := testenv.Command(t, testenv.GoToolPath(t), "test", "-race", "-c", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go test -race -c: %v\n%s", err, out)
	}
	out, _ := sleepChild(t, exe, "", 5000, "")
	wantThrow(t, out, "1000ms")
	out, _ = sleepChild(t, exe, "fastleafcheck=0", 1500, "")
	wantSlept(t, out)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
