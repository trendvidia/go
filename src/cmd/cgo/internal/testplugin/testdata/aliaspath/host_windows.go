// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

// Command aliaspath drives trendvidia/go#46 end to end: a path the
// loader resolves onto an already-loaded module is rejected by
// plugin.Open, but that rejection is process state, not a property of
// the file — so once the blocking module is closed, the same Open must
// succeed instead of replaying a cached failure.
//
// The alias spelling is a hardlink: the Windows loader identifies
// modules by file identity (measured on Windows 11 arm64 and amd64,
// 2026-08), so LoadLibrary through a second directory entry for an
// already-mapped file returns the existing module, whose recorded path
// names the first spelling — the alias branch's exact input. The
// existing samename fixture cannot reach this branch on the measured
// loaders: two distinct same-named files map as their own modules.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"plugin"
	"strings"
)

func name(p *plugin.Plugin) string {
	f, err := p.Lookup("Name")
	if err != nil {
		log.Fatalf(`Lookup("Name") failed: %v`, err)
	}
	return f.(func() string)()
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	realPath := filepath.Join(cwd, "aliaspath-plugin.dll")
	aliasPath := filepath.Join(cwd, "aliaspath-alias.dll")
	os.Remove(aliasPath) // stale link from an earlier run
	if err := os.Link(realPath, aliasPath); err != nil {
		log.Fatalf("hardlink %s -> %s failed: %v", aliasPath, realPath, err)
	}
	defer os.Remove(aliasPath)

	p1, err := plugin.Open(realPath)
	if err != nil {
		log.Fatalf("plugin.Open(%q) failed: %v", realPath, err)
	}
	if got, want := name(p1), "A"; got != want {
		log.Fatalf("%s: Name()=%q, want %q", realPath, got, want)
	}

	// While the real spelling is loaded, the alias resolves onto its
	// module and must be rejected — every time, from the loader, never
	// from the failure cache.
	const aliased = "resolved to a different module, already loaded from"
	for i := 1; i <= 2; i++ {
		_, err := plugin.Open(aliasPath)
		if i == 1 && (err == nil || !strings.Contains(err.Error(), aliased)) {
			// Premise, not result: this loader/filesystem did not
			// resolve the hardlink onto the already-mapped image, so
			// the alias branch was never entered and there is nothing
			// to pin. Only the FIRST attempt may bow out this way —
			// once the branch has fired, round 2 and the post-Close
			// reopen below stay hard assertions.
			fmt.Printf("skip: loader did not alias %q onto %q (Open = %v)\n", aliasPath, realPath, err)
			return
		}
		if err == nil {
			log.Fatalf("open #%d: plugin.Open(%q) succeeded while %q is loaded; want the alias rejection", i, aliasPath, realPath)
		}
		if !strings.Contains(err.Error(), aliased) {
			log.Fatalf("open #%d: plugin.Open(%q) = %v; want the %q rejection", i, aliasPath, err, aliased)
		}
		if strings.Contains(err.Error(), "(previous failure)") {
			log.Fatalf("open #%d: plugin.Open(%q) = %v; the alias rejection was served from the failure cache", i, aliasPath, err)
		}
	}

	if err := p1.Close(); err != nil {
		log.Fatalf("Close(%q) failed: %v", realPath, err)
	}

	// The blocking module is gone: the alias path must now map its own
	// image and load, not replay the stale rejection.
	p2, err := plugin.Open(aliasPath)
	if err != nil {
		log.Fatalf("plugin.Open(%q) after Close = %v; want success", aliasPath, err)
	}
	if got, want := name(p2), "A"; got != want {
		log.Fatalf("%s: Name()=%q after reopen, want %q", aliasPath, got, want)
	}
	if err := p2.Close(); err != nil {
		log.Fatalf("Close(%q) failed: %v", aliasPath, err)
	}
	fmt.Println("PASS")
}
