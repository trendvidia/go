// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

#include "textflag.h"

// func clearCache(addr, n uintptr)
//
// Walks [addr, addr+n) in 16-byte steps, which is no larger than any
// arm64 cache line, so every line is covered.
TEXT ·clearCache(SB),NOSPLIT,$0-16
	MOVD	addr+0(FP), R0
	MOVD	n+8(FP), R1
	ADD	R0, R1, R1	// end
	MOVD	R0, R2
dloop:
	WORD	$0xd50b7b22	// DC CVAU, R2
	ADD	$16, R2
	CMP	R1, R2
	BLO	dloop
	WORD	$0xd5033b9f	// DSB ISH
	MOVD	R0, R2
iloop:
	WORD	$0xd50b7522	// IC IVAU, R2
	ADD	$16, R2
	CMP	R1, R2
	BLO	iloop
	WORD	$0xd5033b9f	// DSB ISH
	WORD	$0xd5033fdf	// ISB
	RET
