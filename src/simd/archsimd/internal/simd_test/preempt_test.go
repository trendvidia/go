// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"simd/archsimd"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// preemptChildEnv turns a re-exec of this test binary into the child half of
// TestAsyncPreemptPreservesVectors.
const preemptChildEnv = "GO_SIMD_TEST_PREEMPT_CHILD"

// preemptTestDuration is how long one configuration keeps a vector live while
// the collector preempts it. A GC cycle here takes well under a millisecond,
// so this is a few hundred asynchronous preemptions.
const preemptTestDuration = 300 * time.Millisecond

// TestAsyncPreemptPreservesVectors pins that asynchronous preemption saves and
// restores the widest vector state code on this CPU may have live, keyed on
// every feature that admits such code rather than on one of them.
//
// The check runs in this process for the features the CPU actually reports,
// and again in child processes whose GODEBUG hides features from internal/cpu.
// The children are what give the test teeth on ordinary hardware: keying the
// 256-bit save on HasAVX2 alone loses YMM state whenever the runtime sees AVX
// without AVX2 (a Sandy/Ivy Bridge CPU, or cpu.avx2=off), and keying it on
// HasAVX alone loses YMM state whenever it sees AVX2 without AVX (cpu.avx=off,
// since internal/cpu's GODEBUG options do not cascade). Both configurations
// still execute 256-bit instructions, because what the compiler emitted does
// not change when a GODEBUG hides a flag.
func TestAsyncPreemptPreservesVectors(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to preempt a spinning goroutine")
	}
	// With asynchronous preemption off, the loop below is stopped at the
	// outer iteration's calls instead, and would pass without ever reaching
	// asyncPreempt.
	if strings.Contains(os.Getenv("GODEBUG"), "asyncpreemptoff=1") {
		t.Skip("GODEBUG=asyncpreemptoff=1 disables the mechanism under test")
	}

	t.Run("native", func(t *testing.T) {
		summary, err := vectorsSurvivePreemption()
		if err != nil {
			t.Fatal(err)
		}
		t.Log(summary)
	})

	for _, tc := range []struct {
		name    string
		godebug string
		view    string // the internal/cpu view the GODEBUG must produce
		hw      bool   // whether this CPU can host that view
	}{
		{"avx_only", "cpu.avx2=off,cpu.avx512f=off", "avx=true avx2=false avx512=false", archsimd.X86.AVX()},
		{"avx2_only", "cpu.avx=off,cpu.avx512f=off", "avx=false avx2=true avx512=false", archsimd.X86.AVX2()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.hw {
				t.Skipf("CPU cannot present %q", tc.view)
			}
			runPreemptChild(t, tc.godebug, tc.view)
		})
	}
}

// runPreemptChild re-executes this test binary under godebug and requires the
// child to survive preemption. A child that does not end up with wantView has
// not tested anything, so it skips rather than passing quietly: GOAMD64 >= v3
// drops the cpu.avx and cpu.avx2 options, and on Windows the cpu options never
// apply at all, because runtime.getGodebugEarly does not read the environment
// there and cpu.Initialize runs before it is available.
func runPreemptChild(t *testing.T, godebug, wantView string) {
	t.Helper()
	// os.Executable, not os.Args[0]: on Windows the test binary is often
	// invoked by a bare name, which exec.Command would refuse to resolve
	// from the current directory.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating this test binary: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), preemptChildEnv+"=1", "GODEBUG="+godebug)
	out, err := cmd.CombinedOutput()

	view, ok := "", false
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if v, cut := strings.CutPrefix(line, "view: "); cut {
			view, ok = v, true
		}
	}
	if !ok {
		// The child prints its view before doing anything else, so no view
		// line means it never ran. Failing here rather than skipping is what
		// keeps a child that cannot start from passing as "not applicable".
		t.Fatalf("GODEBUG=%s: child reported no internal/cpu view: %v\nchild output:\n%s", godebug, err, out)
	}
	if view != wantView {
		t.Skipf("GODEBUG=%s gave internal/cpu view %q, want %q; child output:\n%s", godebug, view, wantView, out)
	}
	if err != nil {
		t.Fatalf("GODEBUG=%s: child failed: %v\nchild output:\n%s", godebug, err, out)
	}
	t.Logf("GODEBUG=%s: %s", godebug, strings.TrimSpace(string(out)))
}

// asyncPreemptChildMain runs the vector check and exits if this process is a
// child of TestAsyncPreemptPreservesVectors; otherwise it returns. TestMain
// calls it ahead of the package's AVX gate, which a child running under
// cpu.avx=off would otherwise fail even though the CPU has AVX.
func asyncPreemptChildMain() {
	if os.Getenv(preemptChildEnv) == "" {
		return
	}
	// The parent only reaches here with GOMAXPROCS >= 2, but the child does
	// not inherit -test.cpu, so ask for the second P explicitly.
	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}
	fmt.Printf("view: avx=%v avx2=%v avx512=%v\n",
		archsimd.X86.AVX(), archsimd.X86.AVX2(), archsimd.X86.AVX512())
	summary, err := vectorsSurvivePreemption()
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK:", summary)
	os.Exit(0)
}

// vectorsSurvivePreemption keeps a Float32x8 live in a vector register through
// a call-free loop while another goroutine forces garbage collections, and
// reports an error if any lane comes back changed. On success it returns a
// summary of how much preemption the run actually saw.
//
// Every runtime.GC stops the world, and a call-free loop has no safe point to
// stop at, so it can only be stopped by an asynchronous preemption: a
// completed GC cycle is evidence that asyncPreempt saved and restored the
// register the vector lives in.
func vectorsSurvivePreemption() (string, error) {
	base := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	delta := archsimd.LoadFloat32x8([]float32{0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5})

	var stop atomic.Bool
	var gcs atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
			gcs.Add(1)
		}
	}()
	halt := func() {
		stop.Store(true)
		<-done
	}

	got := make([]float32, 8)
	iters := 0
	for start := time.Now(); time.Since(start) < preemptTestDuration; iters++ {
		// No calls inside this loop: the scheduler cannot preempt it at a
		// call boundary, so every stop-the-world lands as an asynchronous
		// preemption somewhere in the chain, with v live in a vector
		// register.
		v := archsimd.LoadFloat32x8(base)
		for i := 0; i < 1<<18; i++ {
			v = v.Add(delta)
			v = v.Sub(delta)
			v = v.Add(delta)
			v = v.Sub(delta)
		}
		v.Store(got)
		for i := range base {
			if got[i] != base[i] {
				halt()
				return "", fmt.Errorf("iteration %d: lane %d = %v, want %v (got=%v)", iters, i, got[i], base[i], got)
			}
		}
	}
	halt()
	if n := gcs.Load(); n == 0 {
		return "", fmt.Errorf("no GC cycle completed in %v (%d iterations): nothing forced an asynchronous preemption", preemptTestDuration, iters)
	}
	return fmt.Sprintf("%d iterations survived %d GC cycles", iters, gcs.Load()), nil
}
