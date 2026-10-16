// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

package testfastleaf_test

import (
	"cmd/cgo/internal/testfastleaf"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestFastLeafNoEINTR makes fastleaf C calls that sleep, on a goroutine
// whose stack the collector keeps wanting to scan, and counts the sleeps
// a signal interrupted. The runtime does not send its preemption signal
// to a thread in a fastleaf call: the signal cannot stop C code, and it
// makes interruptible system calls fail with EINTR. Without that, every
// sleep here is interrupted.
//
// The count is not always zero: preemptM reads the flag while the
// goroutine is still in Go between calls, and the signal can land after
// the next call has started. Ordinary cgo calls have the same race (a
// signal sent just before entersyscall): measured 0-3 of 200 for
// fastleaf and 0-2 of 200 for ordinary cgo, against 200 of 200 without
// the suppression. Closing the window would mean blocking the signal
// around every call, which costs 20 to 70 times the call itself, so the
// test bounds the residual instead.
func TestFastLeafNoEINTR(t *testing.T) {
	leaf, plain := countEINTR(t, testfastleaf.SleepLeaf), countEINTR(t, testfastleaf.SleepPlain)
	t.Logf("interrupted sleeps out of 200: fastleaf %d, ordinary cgo %d", leaf, plain)
	if leaf > 20 {
		t.Errorf("%d of 200 fastleaf sleeps were interrupted by a signal, more than the 10%% a missed suppression race allows", leaf)
	}
}

func countEINTR(t *testing.T, sleep func(int) bool) int {
	var stop atomic.Bool
	var gcs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			runtime.GC()
			gcs.Add(1)
		}
	}()
	interrupted := 0
	for range 200 {
		if sleep(2_000_000) { // 2ms
			interrupted++
		}
	}
	stop.Store(true)
	wg.Wait()
	// A positive marker: the sleeps overlapped many collections, each of
	// which scans this goroutine's stack.
	if gcs.Load() < 20 {
		t.Fatalf("only %d collections overlapped the sleeps", gcs.Load())
	}
	return interrupted
}
