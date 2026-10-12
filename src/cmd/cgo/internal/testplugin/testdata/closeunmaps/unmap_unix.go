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

// cIsReadable reports whether the byte at addr can be read, WITHOUT
// faulting if it cannot. write() to a pipe makes the kernel do the
// read on our behalf and report EFAULT instead of delivering a signal.
//
// Returns: 1 = readable, 0 = not readable (EFAULT), -1 = inconclusive.
static int cIsReadable(uintptr_t addr) {
	int fds[2];
	if (pipe(fds) != 0) {
		return -1;
	}
	errno = 0;
	ssize_t n = write(fds[1], (void*)addr, 1);
	int e = errno;
	close(fds[0]);
	close(fds[1]);
	if (n == 1) {
		return 1;
	}
	if (n < 0 && e == EFAULT) {
		return 0;
	}
	return -1;
}

// cIsUnmapped reports whether the plugin's IMAGE is gone from addr —
// which stopped being the same question as "is anything mapped there"
// when unload began reserving the range it freed (trendvidia/go-archive#123).
//
// mincore answers the narrow question: -1/ENOMEM when the address is
// part of no mapping at all. That still means the image is gone.
//
// The reservation makes the range a PROT_NONE anonymous mapping, so
// mincore now succeeds there and would report "mapped" for memory that
// holds nothing and faults on every access. Distinguish it by
// readability rather than by presence: a loaded image's text is
// readable, and both a PROT_NONE reservation and an unmapped hole are
// not. That is the same property the Windows probe tests when it
// accepts MEM_RESERVE but rejects a committed MEM_IMAGE region.
//
// Returns: 1 = image gone (no mapping, or an unreadable one),
// 0 = still mapped and readable, -1 = unexpected errno (in *errnoOut).
static int cIsUnmapped(uintptr_t addr, int *errnoOut) {
	long ps = sysconf(_SC_PAGESIZE);
	unsigned char vec[1];
	uintptr_t page = pageOf(addr);
	if (mincore((void*)page, ps, (char*)vec) != 0) {
		if (errno == ENOMEM) {
			return 1; // no mapping at all
		}
		*errnoOut = errno;
		return -1;
	}
	switch (cIsReadable(addr)) {
	case 0:
		return 1; // mapped but inaccessible: the reservation
	case 1:
		return 0; // readable: the image is still there
	}
	*errnoOut = 0;
	return -1;
}
*/
import "C"

func isUnmapped(addr uintptr) (status int, errnoOut int) {
	var errOut C.int
	got := C.cIsUnmapped(C.uintptr_t(addr), &errOut)
	return int(got), int(errOut)
}
