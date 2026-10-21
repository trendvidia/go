// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"math"
	"runtime"
	"testing"
)

// TestGoplusSampleWeight checks the runtime's weight for a heap sample
// against the formula runtime/pprof scales heap profiles by, computed
// with math.Exp.
func TestGoplusSampleWeight(t *testing.T) {
	// Rates up to the sampler's largest mean (see
	// TestGoplusSampleWeightMean for the rates above it).
	for _, rate := range []int{2, 4096, 64 << 10, 512 << 10, 64 << 20} {
		// Up to 64 GiB, or the largest size a uintptr holds.
		for sz := uint64(1); sz <= 1<<36 && sz == uint64(uintptr(sz)); sz *= 3 {
			size := uintptr(sz)
			got := runtime.GoplusSampleWeight(size, rate)
			want := float64(size) / -math.Expm1(-float64(size)/float64(rate))
			if math.Abs(float64(got)-want) > 1+want*1e-9 {
				t.Errorf("weight(size %d, rate %d) = %d, want %.1f", size, rate, got, want)
			}
		}
	}
	for _, size := range []uintptr{1, 8, 1 << 20} {
		if got := runtime.GoplusSampleWeight(size, 1); got != int64(size) {
			t.Errorf("weight(size %d, rate 1) = %d, want the size", size, got)
		}
	}
}

// TestGoplusSampleWeightMean checks that a sample is weighted by the mean
// the sampler actually draws its gaps from, measured from the draws, and
// not by MemProfileRate where the two differ: the sampler caps its mean,
// so weighting by a higher rate would inflate every sample.
func TestGoplusSampleWeightMean(t *testing.T) {
	const draws = 200000 // the mean's relative standard error is 0.22%
	for _, rate := range []int{512 << 10, 1 << 28, 1 << 30, math.MaxInt32} {
		var sum float64
		for range draws {
			sum += float64(runtime.Fastexprand(rate))
		}
		mean := sum / draws
		for _, size := range []uintptr{16, 4096, 1 << 20, 64 << 20} {
			got := float64(runtime.GoplusSampleWeight(size, rate))
			want := float64(size) / -math.Expm1(-float64(size)/mean)
			if math.Abs(got-want) > want*0.02 {
				t.Errorf("weight(size %d, rate %d) = %.0f, want %.0f for the sampler's measured mean gap %.0f",
					size, rate, got, want, mean)
			}
		}
	}
}
