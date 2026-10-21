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
	for _, rate := range []int{2, 4096, 64 << 10, 512 << 10, 1 << 30} {
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
