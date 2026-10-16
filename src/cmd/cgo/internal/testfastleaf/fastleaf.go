// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf

/*
#cgo noescape addNoCB
#cgo nocallback addNoCB
#cgo fastleaf addLeaf
#cgo fastleaf sumLeaf
#cgo fastleaf callsBack
#cgo fastleaf spinLeaf
#cgo fastleaf crashLeaf

#include <stddef.h>
#include <stdint.h>

static int addPlain(int x) { return x + 1; }
static int addNoCB(int x) { return x + 1; }
static int addLeaf(int x) { return x + 1; }

static uint64_t sumLeaf(const unsigned char *p, size_t n) {
	uint64_t s = 0;
	for (size_t i = 0; i < n; i++) {
		s = s * 31 + p[i];
	}
	return s;
}

// spinLeaf busy-loops for about n iterations, long enough for profiling
// signals to land in it.
static uint64_t spinLeaf(int n) {
	volatile uint64_t s = 0;
	for (int i = 0; i < n; i++) {
		s += (uint64_t)i * 2654435761u;
	}
	return s;
}

// crashLeaf faults on a NULL write.
static void crashLeaf(void) {
	*(volatile int *)0 = 1;
}

extern void goCallback(void);
static void callsBack(void) { goCallback(); }
*/
import "C"

import "unsafe"

func AddPlain(x int) int { return int(C.addPlain(C.int(x))) }
func AddNoCB(x int) int  { return int(C.addNoCB(C.int(x))) }
func AddLeaf(x int) int  { return int(C.addLeaf(C.int(x))) }

//go:noinline
func AddGo(x int) int { return x + 1 }

// SumLeaf hashes b in C, reading Go memory through the pointer.
func SumLeaf(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return uint64(C.sumLeaf((*C.uchar)(unsafe.Pointer(&b[0])), C.size_t(len(b))))
}

// SumGo is SumLeaf's reference.
func SumGo(b []byte) uint64 {
	var s uint64
	for _, c := range b {
		s = s*31 + uint64(c)
	}
	return s
}

// SpinLeaf runs a fastleaf C function that spins for about n iterations.
func SpinLeaf(n int) uint64 { return uint64(C.spinLeaf(C.int(n))) }

// CrashLeaf calls a fastleaf C function that faults.
func CrashLeaf() { C.crashLeaf() }

// CallsBack calls a fastleaf C function that calls back into Go, which
// must throw.
func CallsBack() { C.callsBack() }

//export goCallback
func goCallback() {}
