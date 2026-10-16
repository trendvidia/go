// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf_test

import (
	"cmd/cgo/internal/testfastleaf"
	"internal/testenv"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFastLeafResults(t *testing.T) {
	for _, x := range []int{-1, 0, 41, 1 << 20} {
		if got := testfastleaf.AddLeaf(x); got != x+1 {
			t.Fatalf("AddLeaf(%d) = %d", x, got)
		}
	}
}

// TestFastLeafHeapMemory passes Go heap memory to a fastleaf function
// while collections run back to back, and checks every result against
// the same computation in Go.
func TestFastLeafHeapMemory(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	var gcs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
				gcs.Add(1)
			}
		}
	}()
	iters := 20000
	if testing.Short() {
		iters = 4000
	}
	r := rand.New(rand.NewPCG(1, 2))
	var garbage [][]byte
	// Run for at least iters calls and at least 10 collections: bounding
	// by calls alone ended a -short run after 6 collections on a fast CI
	// runner.
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; i < iters || gcs.Load() < 10; i++ {
		if time.Now().After(deadline) {
			break
		}
		b := make([]byte, 1+r.IntN(4096))
		for j := range b {
			b[j] = byte(r.Uint32())
		}
		if got, want := testfastleaf.SumLeaf(b), testfastleaf.SumGo(b); got != want {
			t.Fatalf("iteration %d: fastleaf sum of %d bytes = %#x, want %#x", i, len(b), got, want)
		}
		garbage = append(garbage, make([]byte, 512))
		if len(garbage) > 64 {
			garbage = nil
		}
	}
	close(stop)
	wg.Wait()
	// A positive marker that the calls overlapped collections.
	if gcs.Load() < 10 {
		t.Fatalf("only %d collections ran during the test", gcs.Load())
	}
}

// TestFastLeafStopTheWorld runs goroutines making short fastleaf calls on
// every P and checks that the world still stops promptly, next to the
// same measurement for goroutines in a pure Go loop.
func TestFastLeafStopTheWorld(t *testing.T) {
	const procs = 4
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
	goLoops := measureStops(t, procs, func() { testfastleaf.AddGo(1) })
	leafLoops := measureStops(t, procs, func() { testfastleaf.AddLeaf(1) })
	t.Logf("slowest stop-the-world with %d spinning goroutines: Go calls under %gs, fastleaf calls under %gs", procs, goLoops, leafLoops)
	// The bound allows for a loaded machine; a fastleaf call that never
	// let the world stop would exceed it.
	if leafLoops > 1 {
		t.Errorf("stopping the world took up to %gs with fastleaf callers on every P", leafLoops)
	}
}

// measureStops runs n goroutines calling call in a loop, runs five
// collections, and returns the upper bound of the slowest recorded time
// to stop the world for one.
func measureStops(t *testing.T, n int, call func()) float64 {
	t.Helper()
	var stop atomic.Bool
	var calls atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				call()
				calls.Add(1)
			}
		}()
	}
	defer func() { stop.Store(true); wg.Wait() }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	stopping := []metrics.Sample{{Name: "/sched/pauses/stopping/gc:seconds"}}
	metrics.Read(stopping)
	before := slices.Clone(stopping[0].Value.Float64Histogram().Counts)
	for range 5 {
		progress := calls.Load()
		runtime.GC()
		if calls.Load() == progress {
			t.Fatal("the callers made no progress across a collection")
		}
	}
	metrics.Read(stopping)
	after := stopping[0].Value.Float64Histogram()
	stops, worst := 0, 0.0
	for i, c := range after.Counts {
		if d := c - before[i]; d > 0 {
			stops += int(d)
			worst = max(worst, after.Buckets[i+1])
		}
	}
	if stops < 10 {
		t.Fatalf("only %d stop-the-world pauses recorded for 5 collections", stops)
	}
	return worst
}

// TestFastLeafCallbackThrows checks that a fastleaf function calling back
// into Go stops the program with a message naming the directive.
func TestFastLeafCallbackThrows(t *testing.T) {
	if os.Getenv("FASTLEAF_CALLBACK_CHILD") != "" {
		testfastleaf.CallsBack()
		t.Fatal("CallsBack returned")
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestFastLeafCallbackThrows$")
	cmd.Env = append(os.Environ(), "FASTLEAF_CALLBACK_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited cleanly:\n%s", out)
	}
	if want := "function marked with #cgo fastleaf called back into Go"; !strings.Contains(string(out), want) {
		t.Fatalf("child output lacks %q:\n%s", want, out)
	}
}

// BenchmarkCall compares the cost of calling a trivial C function through
// plain cgo, with noescape and nocallback, and with fastleaf, against a
// Go function call.
func BenchmarkCall(b *testing.B) {
	for _, bc := range []struct {
		name string
		f    func(int) int
	}{
		{"go", testfastleaf.AddGo},
		{"cgo", testfastleaf.AddPlain},
		{"cgo-noescape-nocallback", testfastleaf.AddNoCB},
		{"cgo-fastleaf", testfastleaf.AddLeaf},
	} {
		b.Run(bc.name, func(b *testing.B) {
			x := 0
			for b.Loop() {
				x = bc.f(x)
			}
		})
	}
}
