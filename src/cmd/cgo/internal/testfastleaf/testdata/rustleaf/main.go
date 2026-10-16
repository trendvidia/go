// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Program rustleaf calls a Rust function through #cgo fastleaf. Its
// static library and link flags are supplied by the test.
package main

/*
#cgo fastleaf rust_sum
#include <stddef.h>
#include <stdint.h>
int rust_sum(const unsigned char *p, size_t n, uint64_t *out);
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"
)

func sum(b []byte) (uint64, bool) {
	var out C.uint64_t
	n := C.size_t(len(b))
	if b == nil {
		n = ^C.size_t(0) // asks rust_sum to panic
		b = []byte{0}
	}
	rc := C.rust_sum((*C.uchar)(unsafe.Pointer(&b[0])), n, &out)
	return uint64(out), rc == 0
}

func main() {
	b := make([]byte, 1<<16)
	var want uint64
	for i := range b {
		b[i] = byte(i * 7)
		want = want*31 + uint64(b[i])
	}
	for range 1000 {
		got, ok := sum(b)
		if !ok || got != want {
			fmt.Printf("FAIL: sum = %#x, %v; want %#x\n", got, ok, want)
			os.Exit(1)
		}
		runtime.GC()
	}
	if _, ok := sum(nil); ok {
		fmt.Println("FAIL: a panic in Rust was not reported")
		os.Exit(1)
	}
	fmt.Println("OK: 1000 fastleaf calls into Rust; a Rust panic was caught at the boundary")
}
