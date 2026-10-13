// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBoringCachesRegistrantsAreNotPlatformGated pins the reach of the
// runtime.boringCaches hazard recorded at peSharedRuntimeData
// (trendvidia/go-archive#68): a plugin-only registrant puts plugin-resident data into
// the host-shared slice, and clearpools stores nil through it at the first GC
// after Plugin.Close unmaps the image — the runtime.poolcleanup shape of trendvidia/go-archive#61.
//
// trendvidia/go-archive#68 recorded that as out of scope because crypto/internal/boring carries
// `//go:build boringcrypto && linux && (amd64 || arm64) && !android && !msan`,
// so no Windows boringcrypto build could exist. That premise is false, and
// this test is what says so out loud. That constraint gates the cgo BoringSSL
// binding — whether the cache is USED. What gets it REGISTERED is
// crypto/rsa's and crypto/ecdsa's init, gated on `boringcrypto` alone, calling
// into crypto/internal/boring/bcache, which carries no constraint at all.
// `GOEXPERIMENT=boringcrypto GOOS=windows go build` therefore links
// crypto/rsa.pubCache, bcache.registerCache and runtime.boringCaches today.
//
// This checks the constraints in the source, not runtime.GOOS, on purpose: it
// should fire on the commit that moves them, not months later when a Windows
// boringcrypto plugin faults in clearpools. It fails in either direction —
// narrowing the registrants to linux would make trendvidia/go-archive#68's premise true, which is
// just as much a reason to revisit the decision as widening them further.
func TestBoringCachesRegistrantsAreNotPlatformGated(t *testing.T) {
	t.Parallel()

	goroot := testenv.GOROOT(t)

	// bcache holds the registration itself and is linked wherever a
	// registrant is; a constraint appearing here would narrow the hazard.
	cache := filepath.Join(goroot, "src", "crypto", "internal", "boring", "bcache", "cache.go")
	if c := buildConstraint(t, cache); c != "" {
		t.Errorf("crypto/internal/boring/bcache is now constrained:\n"+
			"  //go:build %s\n\n"+
			"The runtime.boringCaches entry in peSharedRuntimeData (pe_implib.go) "+
			"describes the hazard as reachable wherever the boringcrypto tag is "+
			"set. Re-measure that description and revisit trendvidia/go-archive#68.", c)
	}

	for _, pkg := range []string{"rsa", "ecdsa"} {
		path := filepath.Join(goroot, "src", "crypto", pkg, "boring.go")
		src := readSource(t, path)

		if c := constraintOf(src); c != "boringcrypto" {
			t.Errorf("crypto/%s/boring.go is no longer gated on boringcrypto alone:\n"+
				"  //go:build %s\n\n"+
				"That constraint is what decides whether a plugin-only "+
				"crypto/%s can register plugin-resident data into host-shared "+
				"runtime.boringCaches and dangle after Plugin.Close. The "+
				"boringCaches entry in peSharedRuntimeData (pe_implib.go) states "+
				"its reach; re-measure it and revisit trendvidia/go-archive#68 — which is "+
				"still open on whether to unregister at Close or refuse the "+
				"configuration at link time.", pkg, c, pkg)
		}

		// The dangle needs the pointer to reach the runtime at all, and
		// nothing but init does that: Cache.Register documents that it
		// "must be called during package initialization".
		if !strings.Contains(src, ".Register()") {
			t.Errorf("crypto/%s/boring.go no longer calls Cache.Register; the "+
				"runtime.boringCaches hazard described in pe_implib.go may no "+
				"longer have a registrant. Re-measure and revisit trendvidia/go-archive#68.", pkg)
		}
	}
}

// readSource returns the contents of path, failing the test if it cannot be
// read. A pin that skips when its subject moves is a pin that disappears on
// exactly the change it exists to catch.
func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nThe runtime.boringCaches scope note in "+
			"pe_implib.go rests on this file's build constraint; if the file moved, "+
			"re-measure the note and revisit trendvidia/go-archive#68.", path, err)
	}
	return string(b)
}

// buildConstraint returns the //go:build expression of the file at path, or ""
// if it has none.
func buildConstraint(t *testing.T, path string) string {
	t.Helper()
	return constraintOf(readSource(t, path))
}

// constraintOf returns the //go:build expression preceding src's package
// clause, or "" if there is none.
func constraintOf(src string) string {
	for line := range strings.SplitSeq(src, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "//go:build "); ok {
			return strings.TrimSpace(after)
		}
		if strings.HasPrefix(line, "package ") {
			return ""
		}
	}
	return ""
}
