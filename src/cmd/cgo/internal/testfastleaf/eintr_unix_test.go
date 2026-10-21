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
	"time"
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
	leaf, leafN := countEINTR(t, testfastleaf.SleepLeaf)
	plain, plainN := countEINTR(t, testfastleaf.SleepPlain)
	t.Logf("interrupted sleeps: fastleaf %d of %d, ordinary cgo %d of %d", leaf, leafN, plain, plainN)
	if leaf*10 > leafN {
		t.Errorf("%d of %d fastleaf sleeps were interrupted by a signal, more than the 10%% a missed suppression race allows", leaf, leafN)
	}
}

// countEINTR makes 2ms sleeps until it has made at least 200 and at
// least 20 collections have overlapped them, and returns how many were
// interrupted, out of how many. Waiting for the collections rather than
// for a fixed number of sleeps keeps a slow, loaded machine from
// failing the marker (trendvidia/go#81).
func countEINTR(t *testing.T, sleep func(int) bool) (interrupted, sleeps int) {
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
	deadline := time.Now().Add(time.Minute)
	for sleeps < 200 || gcs.Load() < 20 {
		if time.Now().After(deadline) {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("after %d sleeps only %d collections had overlapped them", sleeps, gcs.Load())
		}
		if sleep(2_000_000) { // 2ms
			interrupted++
		}
		sleeps++
	}
	stop.Store(true)
	wg.Wait()
	return interrupted, sleeps
}
