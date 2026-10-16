// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf_test

import (
	"bytes"
	"cmd/cgo/internal/testfastleaf"
	"internal/profile"
	"internal/testenv"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFastLeafCPUProfile profiles goroutines that spend nearly all their
// time inside a fastleaf C function, and checks that the samples keep
// the Go stack of the call: the profiler must unwind the goroutine from
// where its Go stack stops, not attribute the time to external code.
func TestFastLeafCPUProfile(t *testing.T) {
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	deadline := time.Now().Add(time.Second)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				testfastleaf.SpinLeaf(20000)
			}
		}()
	}
	wg.Wait()
	pprof.StopCPUProfile()
	p, err := profile.Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var total, withCaller, external int64
	for _, s := range p.Sample {
		v := s.Value[0]
		total += v
		var names []string
		for _, loc := range s.Location {
			for _, l := range loc.Line {
				names = append(names, l.Function.Name)
			}
		}
		stack := strings.Join(names, " < ")
		switch {
		case strings.Contains(stack, "testfastleaf.SpinLeaf"):
			withCaller += v
		case strings.Contains(stack, "runtime._ExternalCode"):
			external += v
		}
	}
	t.Logf("%d samples: %d with the Go caller, %d attributed to external code", total, withCaller, external)
	if total < 50 {
		t.Fatalf("only %d samples; the profile did not run long enough to judge", total)
	}
	if withCaller < total/2 {
		t.Errorf("only %d of %d samples during fastleaf calls kept the Go caller's stack", withCaller, total)
	}
}

// TestFastLeafCrashInC checks that a fault inside a fastleaf C function
// produces an ordinary crash report: the signal, and the stack of the Go
// code that made the call.
func TestFastLeafCrashInC(t *testing.T) {
	if os.Getenv("FASTLEAF_CRASH_CHILD") != "" {
		testfastleaf.CrashLeaf()
		t.Fatal("CrashLeaf returned")
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestFastLeafCrashInC$")
	cmd.Env = append(os.Environ(), "FASTLEAF_CRASH_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited cleanly:\n%s", out)
	}
	s := string(out)
	for _, bad := range []string{"panic during panic", "unknown pc", "fatal error: unsafe.Slice"} {
		if strings.Contains(s, bad) {
			t.Errorf("crash report contains %q: the report itself failed:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "testfastleaf.CrashLeaf") {
		t.Errorf("crash report lacks the Go caller testfastleaf.CrashLeaf:\n%s", s)
	}
	if !strings.Contains(s, "SIGSEGV") && !strings.Contains(s, "Exception 0xc0000005") {
		t.Errorf("crash report does not name the fault:\n%s", s)
	}
}
