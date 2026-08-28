// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"simd/archsimd"
	"testing"
)

func TestFMA(t *testing.T) {
	// The 128- and 256-bit forms declare FMA, the 512-bit forms AVX512;
	// guard on the feature each declares rather than on vector width.
	if archsimd.X86.FMA() {
		testFloat32x4TernaryFlaky(t, archsimd.Float32x4.MulAdd, fmaSlice[float32], 0.001)
		testFloat32x8TernaryFlaky(t, archsimd.Float32x8.MulAdd, fmaSlice[float32], 0.001)
		testFloat64x2Ternary(t, archsimd.Float64x2.MulAdd, fmaSlice[float64])
		testFloat64x4Ternary(t, archsimd.Float64x4.MulAdd, fmaSlice[float64])
	}
	if archsimd.X86.AVX512() {
		testFloat32x16TernaryFlaky(t, archsimd.Float32x16.MulAdd, fmaSlice[float32], 0.001)
		testFloat64x8Ternary(t, archsimd.Float64x8.MulAdd, fmaSlice[float64])
	}
}
