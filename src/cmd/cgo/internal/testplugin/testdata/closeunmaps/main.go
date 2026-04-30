// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises v2's dlclose-at-Close: after Plugin.Close, the kernel
// has unmapped the plugin's text pages, observable as mincore
// returning ENOMEM (POSIX) or VirtualQuery reporting MEM_FREE
// (Windows) on the captured pre-Close code address.
//
// The page-mapping check is the rock-solid signal: a passing
// TestPanicOnCallStub only proves the runtime rewrote the held
// descriptor before unmap, not that the unmap actually happened.
// This fixture proves the unmap.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"unsafe"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	p, err := plugin.Open("closeunmaps-plugin1" + pluginExt)
	if err != nil {
		fail("Open(closeunmaps-plugin1): %v", err)
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
	if got, errOut := isUnmapped(codeAddr); got != 0 {
		fail("pre-Close isUnmapped(0x%x) = %d err=%d; want 0 (mapped)", codeAddr, got, errOut)
	}

	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}

	// Post-Close mapping check. The strictness is OS-policy-bound:
	//
	//   - Linux: dlclose with refcount==0 reliably triggers munmap,
	//     so the page MUST report ENOMEM. We assert that.
	//
	//   - Windows: FreeLibrary releases the mapping and VirtualQuery
	//     reports MEM_FREE for the formerly-mapped region. Strict
	//     assertion, like Linux.
	//
	//   - Darwin: dyld may retain dylibs in process memory after
	//     dlclose for caching reasons even when refcount drops to
	//     zero (Apple documents dlclose as "may unload"). We can
	//     only assert the weaker contract: dlclose returned 0 (no
	//     error from p.Close above). The OS-level unmap is best
	//     effort. The runtime side of v2 — descriptor stubs +
	//     module unlink — does its job either way; held callers
	//     panic via TestPanicOnCallStub regardless of whether the
	//     mapping survives.
	got, errOut := isUnmapped(codeAddr)
	switch runtime.GOOS {
	case "darwin":
		// Either result is acceptable. Just verify isUnmapped
		// itself didn't error out unexpectedly.
		if got == -1 {
			fail("post-Close: isUnmapped(0x%x) failed with unexpected err=%d", codeAddr, errOut)
		}
	default:
		switch got {
		case 1:
			// Expected.
		case 0:
			fail("post-Close: page containing 0x%x is still mapped; unload did not unmap", codeAddr)
		default:
			fail("post-Close: isUnmapped(0x%x) failed with unexpected err=%d", codeAddr, errOut)
		}
	}

	fmt.Println("closeunmaps ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "closeunmaps: "+format+"\n", args...)
	os.Exit(1)
}
