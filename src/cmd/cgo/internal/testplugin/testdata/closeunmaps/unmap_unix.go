// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package main

/*
#include <errno.h>
#include <stdint.h>
#include <sys/mman.h>
#include <unistd.h>

// pageOf returns the page-aligned base address containing addr.
static uintptr_t pageOf(uintptr_t addr) {
	long ps = sysconf(_SC_PAGESIZE);
	return addr & ~((uintptr_t)ps - 1);
}

// cIsUnmapped probes a single page at addr with mincore. Linux and
// Darwin both return -1/ENOMEM when the queried range is not part
// of any mapping; other errno values mean the mapping exists (so
// the page is mapped — the opposite of what we want).
//
// Returns: 1 = unmapped, 0 = mapped, -1 = unexpected errno (with
// the value placed in *errnoOut).
static int cIsUnmapped(uintptr_t addr, int *errnoOut) {
	long ps = sysconf(_SC_PAGESIZE);
	unsigned char vec[1];
	uintptr_t page = pageOf(addr);
	if (mincore((void*)page, ps, (char*)vec) == 0) {
		return 0; // mapped
	}
	if (errno == ENOMEM) {
		return 1; // unmapped
	}
	*errnoOut = errno;
	return -1;
}
*/
import "C"

func isUnmapped(addr uintptr) (status int, errnoOut int) {
	var errOut C.int
	got := C.cIsUnmapped(C.uintptr_t(addr), &errOut)
	return int(got), int(errOut)
}
