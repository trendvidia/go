// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises v2's dlclose-at-Close: after Plugin.Close, the kernel
// has unmapped the plugin's text pages, observable as mincore
// returning ENOMEM on the captured pre-Close code address.
//
// The page-mapping check is the rock-solid signal: a passing
// TestPanicOnCallStub only proves the runtime rewrote the held
// descriptor before unmap, not that the unmap actually happened.
// This fixture proves the unmap.
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

// pageSize exposes the runtime page size to Go (sysconf doesn't
// translate cleanly through cgo on every platform).
static long pageSize(void) {
	return sysconf(_SC_PAGESIZE);
}

// isUnmapped probes a single page at addr with mincore. Linux and
// Darwin both return -1/ENOMEM when the queried range is not part
// of any mapping; other errno values mean the mapping exists (so
// the page is mapped — the opposite of what we want).
//
// Returns: 1 = unmapped, 0 = mapped, -1 = unexpected errno (with
// the value placed in *errnoOut).
static int isUnmapped(uintptr_t addr, int *errnoOut) {
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

import (
	"fmt"
	"os"
	"plugin"
	"unsafe"
)

func main() {
	p, err := plugin.Open("plugin1.so")
	if err != nil {
		fail("Open(plugin1.so): %v", err)
	}
	sym, err := p.Lookup("F")
	if err != nil {
		fail("Lookup(F): %v", err)
	}
	f, ok := sym.(func() int)
	if !ok {
		fail("Lookup(F) is %T; want func() int", sym)
	}

	// Capture the dlsym'd code address before Close. plugin.Lookup
	// wraps the address in a heap holder; the func value's data
	// pointer is the holder, and the holder's value is the code
	// pointer. After Close the runtime overwrites that value with
	// the panic stub PC, so we must read it now.
	holderAddr := *(*unsafe.Pointer)(unsafe.Pointer(&f))
	codeAddr := *(*uintptr)(holderAddr)

	// Sanity: the code address must currently be mapped — anything
	// else means we read the wrong word.
	var errnoOut C.int
	if got := C.isUnmapped(C.uintptr_t(codeAddr), &errnoOut); got != 0 {
		fail("pre-Close isUnmapped(0x%x) = %d errno=%d; want 0 (mapped)", codeAddr, int(got), int(errnoOut))
	}

	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}

	// Post-Close: the page containing codeAddr must no longer be
	// part of any mapping (mincore -> ENOMEM).
	got := C.isUnmapped(C.uintptr_t(codeAddr), &errnoOut)
	switch got {
	case 1:
		// Expected.
	case 0:
		fail("post-Close: page containing 0x%x is still mapped; dlclose did not unmap", codeAddr)
	default:
		fail("post-Close: isUnmapped(0x%x) failed with unexpected errno=%d", codeAddr, int(errnoOut))
	}

	fmt.Println("closeunmaps ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "closeunmaps: "+format+"\n", args...)
	os.Exit(1)
}
