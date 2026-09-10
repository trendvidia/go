// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd

import (
	"internal/cpu"
	"simd/archsimd"
)

const archHasHwClmul = true

// archMaxVectorSize reports the widest vector the hardware can hold and the
// widest at which every operation this package bridges to simd/archsimd is
// available. The two differ because the bridge's operations at a width are
// not all in that width's base instruction set: the 128- and 256-bit
// Broadcast* functions are VPBROADCAST (AVX2) and MulAdd is VFMADD (FMA), so a
// CPU with AVX but without AVX2 and FMA, such as Ivy Bridge, must not be given
// hardware SIMD at 128 bits or those operations SIGILL. TestBridgeFeatures
// pins the checks here against the bridge's declarations.
func archMaxVectorSize() (size, allFeatureSize int) {
	if archsimd.X86.AVX() {
		size = 128
		if archsimd.X86.AVX2() && archsimd.X86.FMA() {
			allFeatureSize = 128
		}
	}
	if archsimd.X86.AVX2() {
		size = 256
		if archsimd.X86.FMA() && cpu.X86.HasVPCLMULQDQ {
			allFeatureSize = 256
		}
	}
	if archsimd.X86.AVX512() {
		size = 512
		if cpu.X86.HasAVX512VPCLMULQDQ {
			allFeatureSize = 512
		}
	}
	return
}
