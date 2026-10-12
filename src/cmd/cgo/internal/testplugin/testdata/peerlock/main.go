// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Host for TestPeerVersionLock (trendvidia/go-archive#24): the version lock
// must compare candidate plugins against LOADED PEERS, not just the
// host. peerlock/peerlib is linked into the plugins but not into this
// host, so host-side validation never sees it:
//
//   - pm-plugA loads first and becomes peerlib's canonical owner
//     (main-tree build; Ver() == 11).
//   - pm-plugB carries a DIFFERENT build of the same import path
//     (altpath tree). Its Open must FAIL with the standard named-
//     package error — before #24 it silently bound to A's instance.
//   - pm-plugC carries the SAME build as A. Positive control: it must
//     load cleanly and observe A's instance (Ver() == 11, shared).
//   - A must remain fully usable after B's failed Open.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"strings"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	pa, err := plugin.Open("pm-plugA" + pluginExt)
	if err != nil {
		fail("Open(pm-plugA): %v", err)
	}
	verA := lookupVer(pa)
	if got := verA(); got != 11 {
		fail("plugA Ver() = %d; want 11", got)
	}

	pb, err := plugin.Open("pm-plugB" + pluginExt)
	if err == nil {
		// The hole this test exists for: B loaded and its divergent
		// peerlib silently converged on A's instance. Report what B
		// would have observed, then fail.
		vb := lookupVer(pb)
		fail("Open(pm-plugB) succeeded; want version-lock rejection (B Ver() = %d)", vb())
	}
	if !strings.Contains(err.Error(), "different version of package") ||
		!strings.Contains(err.Error(), "peerlock/peerlib") {
		fail("Open(pm-plugB) error = %q; want named-package version mismatch", err)
	}

	pc, err := plugin.Open("pm-plugC" + pluginExt)
	if err != nil {
		fail("Open(pm-plugC): %v", err)
	}
	if got := lookupVer(pc)(); got != 11 {
		fail("plugC Ver() = %d; want 11 (same-build share with plugA)", got)
	}

	// A survived B's rejection intact.
	if got := verA(); got != 11 {
		fail("post-rejection plugA Ver() = %d; want 11", got)
	}

	fmt.Println("peerlock ok")
}

func lookupVer(p *plugin.Plugin) func() int {
	sym, err := p.Lookup("Ver")
	if err != nil {
		fail("Lookup(Ver) in plugin: %v", err)
	}
	f, ok := sym.(func() int)
	if !ok {
		fail("Ver is %T; want func() int", sym)
	}
	return f
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "peerlock: "+format+"\n", args...)
	os.Exit(1)
}
