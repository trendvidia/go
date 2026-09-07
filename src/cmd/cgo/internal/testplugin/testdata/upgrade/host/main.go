// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests what a pointer held across an extension UPGRADE does
// (trendvidia/go#122). Upgrading is close(v1) then open(v2), which is
// the flow unloading exists to serve, so it is the flow that has to be
// safe.
//
// The loader reuses a freed image's base address despite ASLR, so
// before the fix the stale pointer did not dangle detectably: it
// addressed whatever the NEW image placed at that offset. v2 here
// deliberately moves its fields, so the host writing what it believes
// is Threshold silently sets Budget instead — a wrong value applied to
// live state, with no error anywhere.
//
// Windows reserves the freed range on unload, so the stale write now
// faults. POSIX does not yet (see the pinned verdict below): reserving
// there needs MAP_FIXED, which would clobber a still-mapped image
// rather than fail, so it needs per-OS handling.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	v1, err := plugin.Open("upgrade-v1" + pluginExt)
	if err != nil {
		fail("Open(v1): %v", err)
	}
	sym, err := v1.Lookup("Threshold")
	if err != nil {
		fail("v1.Lookup(Threshold): %v", err)
	}
	stale, ok := sym.(*int)
	if !ok {
		fail("v1 Threshold is %T, want *int", sym)
	}
	*stale = 42

	if err := v1.Close(); err != nil {
		fail("Close(v1): %v", err)
	}

	v2, err := plugin.Open("upgrade-v2" + pluginExt)
	if err != nil {
		fail("Open(v2): %v", err)
	}
	report, err := v2.Lookup("Report")
	if err != nil {
		fail("v2.Lookup(Report): %v", err)
	}
	readV2 := report.(func() (int, int))

	if th, bu := readV2(); th != 7 || bu != 2000 {
		fail("v2 loaded with Threshold=%d Budget=%d, want 7 and 2000", th, bu)
	}

	// The host believes it is retuning the strategy. On Windows the
	// range v1 occupied is reserved, so this faults and the process
	// dies — which is the point: loud beats silent.
	*stale = 999

	th, bu := readV2()
	switch {
	case th == 999:
		fail("stale pointer wrote v2's Threshold (%d): the freed image's address was reused", th)
	case bu == 999:
		// The measured POSIX behaviour, pinned rather than fixed.
		fmt.Println("upgrade: stale write corrupted Budget")
	default:
		fail("stale write landed somewhere else in v2 (Threshold=%d Budget=%d)", th, bu)
	}
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "upgrade: "+format+"\n", args...)
	os.Exit(1)
}
