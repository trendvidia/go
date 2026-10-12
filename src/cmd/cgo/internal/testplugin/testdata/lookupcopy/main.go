// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Looks up symbols through a struct copy of an opened Plugin
// (q := *p), before and after the original's Close
// (trendvidia/go-archive#64).
//
// Plugin.Lookup deliberately accepts such a copy while the plugin is
// live: reading a live plugin through a copy is well-defined, and
// it is only Close (and the dependency accessors) that insist on the
// value Open returned. What must not happen is the copy outliving
// the image: after p.Close() the copy still holds the original's
// syms map — whose KEYS are strings the runtime built over the
// plugin's now-unmapped symbol table, so hashing symName against
// them faults — and the pre-clone pluginpath header into RODATA
// that dlclose has unmapped, which the not-found path used to
// format. closePlugin nils syms and clones pluginpath on the
// original only.
//
// The fixture asserts, in order: a copy of a live plugin looks up
// an existing symbol and gets a clean not-found error for a missing
// one; after the original's Close both lookups on the copy return
// an error instead of faulting; the closed original itself still
// errors; a copy taken AFTER Close errors too; and a re-Open of the
// same path does not resurrect the stale copy.
//
// Every "plugin is closed" leg also asserts whether the message
// NAMES the plugin, and that half of the assertion is the load-
// bearing one: naming it means pluginpath was dereferenced, so a
// leg that must not touch the copy's dangling header (the zero
// value, and every copy taken BEFORE Close) requires the path to be
// absent. Asserting only the message would let a regression that
// always formats pluginpath pass here on darwin, where a post-Close
// dangling read returns garbage silently instead of faulting
// (trendvidia/go-archive#64). It prints "lookup on copy: done" as its last
// line so the harness can tell a silent fault from a finished run.
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
	// A value Open never handed out must error, not index a nil
	// map and format an empty path as if it were a plugin.
	var zero plugin.Plugin
	_, err := zero.Lookup("F")
	wantClosedErr("zero value", err, noPath)

	p, err := plugin.Open("lookupcopy-plugin1" + pluginExt)
	if err != nil {
		fail("Open(lookupcopy-plugin1): %v", err)
	}
	q := *p

	// Live copy: reads resolve through the copy exactly as through
	// the original.
	f, err := q.Lookup("F")
	if err != nil {
		fail("live copy Lookup(F): %v", err)
	}
	if got := f.(func() int)(); got != 3 {
		fail("live copy F() = %d, want 3", got)
	}
	if _, err := q.Lookup("NoSuchSymbol"); err == nil {
		fail("live copy Lookup(NoSuchSymbol) = nil error, want not-found")
	} else if !strings.Contains(err.Error(), "not found in plugin") {
		fail("live copy Lookup(NoSuchSymbol) = %q, want a not-found error", err)
	} else {
		fmt.Printf("live copy missing symbol: %v\n", err)
	}

	if err := p.Close(); err != nil {
		fail("Close() of the original: %v", err)
	}

	// The copy now outlives the image. Neither lookup may touch
	// q.syms (unmapped keys) or q.pluginpath (unmapped header).
	// q was copied BEFORE Close, so q.pluginpath is still the
	// pre-clone header into RODATA the image no longer maps: these
	// errors must not name the plugin.
	_, err = q.Lookup("F")
	wantClosedErr("stale copy, existing symbol", err, noPath)
	_, err = q.Lookup("NoSuchSymbol")
	wantClosedErr("stale copy, missing symbol", err, noPath)

	// The original is closed and its path was cloned, so its error
	// may — and should — name the plugin.
	_, err = p.Lookup("F")
	wantClosedErr("closed original", err, wantsPath)

	// A copy taken after Close inherits the cloned path and the
	// closed marker; it must error the same way, path included.
	late := *p
	_, err = late.Lookup("F")
	wantClosedErr("copy taken after Close", err, wantsPath)

	// Re-opening the path is a fresh load with a fresh loaded
	// channel: the stale copy must keep erroring rather than
	// resolve to the new registration.
	p2, err := plugin.Open("lookupcopy-plugin1" + pluginExt)
	if err != nil {
		fail("re-Open(lookupcopy-plugin1): %v", err)
	}
	if p2 == p {
		fail("re-Open returned the closed *Plugin")
	}
	_, err = q.Lookup("F")
	wantClosedErr("stale copy after re-Open", err, noPath)
	if _, err := p2.Lookup("F"); err != nil {
		fail("Lookup(F) on the re-opened plugin: %v", err)
	}
	if err := p2.Close(); err != nil {
		fail("Close() of the re-opened plugin: %v", err)
	}

	fmt.Println("lookup on copy: done")
}

// pathWanted says whether a "plugin is closed" error is expected to
// name the plugin. It is not decoration: naming the plugin means
// lookup dereferenced pluginpath, which is safe only once Close has
// cloned it into host memory.
type pathWanted bool

const (
	noPath    pathWanted = false
	wantsPath pathWanted = true
)

func wantClosedErr(what string, err error, want pathWanted) {
	if err == nil {
		fail("%s: Lookup() returned no error; want error", what)
	}
	if !strings.Contains(err.Error(), "plugin is closed") {
		fail("%s: Lookup() = %q, want a \"plugin is closed\" error", what, err)
	}
	if named := strings.Contains(err.Error(), "plugin1"); bool(want) != named {
		if want {
			fail("%s: Lookup() = %q, want the plugin path named in the error", what, err)
		}
		fail("%s: Lookup() = %q, want NO plugin path in the error "+
			"(naming it means the stale pluginpath was dereferenced)", what, err)
	}
	fmt.Printf("%s: %v\n", what, err)
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "lookupcopy: "+format+"\n", args...)
	os.Exit(1)
}
