// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAliasRejectionNotCached pins trendvidia/go-archive#46: open()'s alias
// rejection — the requested path resolved to a module already loaded
// from somewhere else — must NOT be written into the plugins failure
// cache. Every other cached failure is a property of the file; the
// alias rejection is process state that changes the moment the
// blocking module is closed, so a repeat Open must re-evaluate it.
//
// The blocking module is constructed with a hardlink: the loader
// identifies modules by file identity (measured on Windows 11 arm64
// and amd64, 2026-08), so LoadLibrary through a second directory
// entry for an already-mapped file returns the existing module, whose
// recorded path names the first spelling — exactly the alias branch's
// input. Once every reference is dropped, the same LoadLibrary maps a
// fresh image recorded under the alias spelling.
func TestAliasRejectionNotCached(t *testing.T) {
	real := writeLoadablePE(t, nil)
	alias := filepath.Join(filepath.Dir(real), "alias.dll")
	if err := os.Link(real, alias); err != nil {
		t.Skipf("cannot hardlink next to %s: %v", real, err)
	}

	// Map the real spelling directly so it blocks the alias, standing
	// in for a plugin held open by its original opener.
	h, err := syscall.LoadLibrary(real)
	if err != nil {
		t.Fatal(err)
	}
	freed := false
	defer func() {
		if !freed {
			syscall.FreeLibrary(h)
		}
	}()

	const aliased = "resolved to a different module, already loaded from"
	for i := 1; i <= 2; i++ {
		_, err := open(alias)
		if err == nil || !strings.Contains(err.Error(), aliased) {
			if i == 1 {
				// Premise, not result: this loader/filesystem did not
				// resolve the hardlink onto the already-mapped image
				// (measured to hold on NTFS, Windows 11 arm64 and
				// amd64, 2026-08), so the alias branch was never
				// entered and there is nothing to pin. Skipping only
				// on the FIRST attempt keeps the pin intact: once the
				// branch has fired, round 2 and the post-FreeLibrary
				// checks below stay hard assertions.
				t.Skipf("loader did not alias %q onto %q (open = %v); alias branch unreachable here", alias, real, err)
			}
			t.Fatalf("open #%d (%q) = %v; want the %q rejection", i, alias, err, aliased)
		}
		// The second round is the pin: a cached entry would answer
		// "(previous failure)" instead of re-running the branch.
		if strings.Contains(err.Error(), "(previous failure)") {
			t.Fatalf("open #%d (%q) = %v; the alias rejection was served from the failure cache", i, alias, err)
		}
	}

	// Close the blocking module. The loader drops the mapping (open's
	// alias branch freed the references its own LoadLibrary calls took).
	if err := syscall.FreeLibrary(h); err != nil {
		t.Fatal(err)
	}
	freed = true

	// The formerly aliased path must now be re-evaluated on its own:
	// a fresh map of this image registers no Go module data, which is
	// the "would now load" outcome for a chained-module-free fixture —
	// not the stale alias error naming a module that is gone.
	_, err = open(alias)
	if err == nil {
		t.Fatalf("open(%q) succeeded for a no-module-data image", alias)
	}
	if strings.Contains(err.Error(), aliased) || strings.Contains(err.Error(), "(previous failure)") {
		t.Fatalf("open(%q) = %v after the blocking module was closed; want the path re-evaluated (no-module-data)", alias, err)
	}
	if want := "registered no Go module data"; !strings.Contains(err.Error(), want) {
		t.Fatalf("open(%q) = %v; want an error containing %q", alias, err, want)
	}
}
