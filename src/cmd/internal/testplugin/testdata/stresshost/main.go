// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Stress host for the multi-plugin lifecycle (ROADMAP A2): in each
// round it opens every currently-closed plugin variant in parallel
// (plus duplicate concurrent Opens of already-loaded paths, which
// must dedup to the same handle), hammers the shared counter through
// every open plugin concurrently, verifies the running total, then
// closes a random subset and lets the GC churn before the next
// round. The host imports testplugin/stressshared, so its instance
// is canonical and Close is always permitted (no plugin-to-plugin
// dependency edges).
//
// Prints PASS on success; prints a diagnostic and exits non-zero on
// the first violated invariant.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"plugin"
	"runtime"
	"sync"
	"sync/atomic"

	"testplugin/stressshared"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func fail(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}

func main() {
	nPlugins := flag.Int("plugins", 8, "number of stressp<i> plugin variants")
	rounds := flag.Int("rounds", 5, "open/hammer/close rounds")
	hammers := flag.Int("hammers", 10, "Adds per plugin per round")
	seed := flag.Int64("seed", 1, "rng seed (reproducibility)")
	flag.Parse()
	rng := rand.New(rand.NewSource(*seed))

	var expected atomic.Int64
	plugins := make([]*plugin.Plugin, *nPlugins) // nil = not currently open
	adds := make([]func(int64) int64, *nPlugins)

	// The host uses stressshared.Add (not merely imports the package)
	// so the linker keeps the host's copy and the host owns its
	// canonical entry. Plugins still form plugin-to-plugin dependency
	// edges through symbols the host cannot own — the variants are
	// compiled from identical bodies, so their hash-named unexported
	// type descriptors (type:.<hash> under -dynlink) are shared
	// peer-to-peer with first-loader-wins ownership. Close refusal
	// while dependents exist is therefore CORRECT behavior, and this
	// test asserts the Close/HasDependents contract rather than
	// assuming Close always succeeds.
	stressshared.Add(1)
	expected.Add(1)

	pathOf := func(i int) string { return fmt.Sprintf("stressp%d%s", i, pluginExt) }

	// openOne opens variant i, validates Lookup of the exported var
	// and function, makes one Add, and records the handle. Only ever
	// called for a slot that is currently nil, and never concurrently
	// for the same i, so the slot writes need no locking.
	openOne := func(i int) error {
		p, err := plugin.Open(pathOf(i))
		if err != nil {
			return fmt.Errorf("Open(%s): %v", pathOf(i), err)
		}
		vSym, err := p.Lookup("V")
		if err != nil {
			return fmt.Errorf("stressp%d.Lookup(V): %v", i, err)
		}
		if v := *vSym.(*int); v != i {
			return fmt.Errorf("stressp%d: V=%d, want %d (wrong plugin or clobbered init)", i, v, i)
		}
		addSym, err := p.Lookup("Add")
		if err != nil {
			return fmt.Errorf("stressp%d.Lookup(Add): %v", i, err)
		}
		add := addSym.(func(int64) int64)
		add(int64(i) + 1)
		expected.Add(int64(i) + 1)
		plugins[i] = p
		adds[i] = add
		return nil
	}

	for r := 0; r < *rounds; r++ {
		// Phase 1: open all closed variants in parallel; for already-
		// open variants, issue a duplicate concurrent Open, which must
		// return the existing handle.
		var wg sync.WaitGroup
		errs := make(chan error, *nPlugins)
		for i := 0; i < *nPlugins; i++ {
			wg.Add(1)
			if plugins[i] != nil {
				go func(i int) {
					defer wg.Done()
					p2, err := plugin.Open(pathOf(i))
					if err != nil {
						errs <- fmt.Errorf("dup Open(%s): %v", pathOf(i), err)
						return
					}
					if p2 != plugins[i] {
						errs <- fmt.Errorf("dup Open(%s) returned a different handle", pathOf(i))
					}
				}(i)
				continue
			}
			go func(i int) {
				defer wg.Done()
				if err := openOne(i); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			fail("round %d: %v", r, err)
		}

		// Phase 2: concurrent Add hammer through every open plugin.
		for i := 0; i < *nPlugins; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for k := 0; k < *hammers; k++ {
					adds[i](1)
					expected.Add(1)
				}
			}(i)
		}
		wg.Wait()

		if got, want := stressshared.Total(), expected.Load(); got != want {
			fail("round %d: shared counter = %d, want %d (canonicalisation lost writes)", r, got, want)
		}

		// Phase 3: try to close a random subset. Sequential by design —
		// concurrent Open and Close of the same path is not part of
		// the API contract this test asserts. The contract checked
		// here: Close succeeds iff the plugin has no dependents, and
		// HasDependents agrees with Close's verdict.
		for i := 0; i < *nPlugins; i++ {
			if plugins[i] == nil || rng.Intn(2) != 0 {
				continue
			}
			hasDeps := plugins[i].HasDependents()
			err := plugins[i].Close()
			switch {
			case hasDeps && err == nil:
				fail("round %d: Close(stressp%d) succeeded but HasDependents was true (deps=%v)",
					r, i, plugins[i].Dependents())
			case !hasDeps && err != nil:
				fail("round %d: Close(stressp%d): %v, but HasDependents was false", r, i, err)
			case err == nil:
				plugins[i] = nil
				adds[i] = nil
			}
			// err != nil with dependents: legitimate refusal; the
			// plugin stays open and stays usable next round.
		}
		runtime.GC()
	}

	// Drain: close everything in dependency order. Each sweep closes
	// every plugin that currently has no dependents; a sweep that
	// makes no progress while plugins remain open means a dependency
	// cycle or a leaked edge — fail loudly.
	for remaining := func() int {
		n := 0
		for _, p := range plugins {
			if p != nil {
				n++
			}
		}
		return n
	}; remaining() > 0; {
		progress := false
		for i := 0; i < *nPlugins; i++ {
			if plugins[i] == nil || plugins[i].HasDependents() {
				continue
			}
			if err := plugins[i].Close(); err != nil {
				fail("drain: Close(stressp%d): %v, but HasDependents was false", i, err)
			}
			plugins[i] = nil
			adds[i] = nil
			progress = true
		}
		if !progress {
			open := []int{}
			for i, p := range plugins {
				if p != nil {
					open = append(open, i)
				}
			}
			fail("drain: no closeable plugin among open set %v (dependency cycle or leaked edge)", open)
		}
	}
	runtime.GC()

	// Reopen everything for a final pass so every variant ends the
	// test having survived at least one close→reopen cycle candidate.
	var wg sync.WaitGroup
	errs := make(chan error, *nPlugins)
	for i := 0; i < *nPlugins; i++ {
		if plugins[i] != nil {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := openOne(i); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		fail("final reopen: %v", err)
	}

	if got, want := stressshared.Total(), expected.Load(); got != want {
		fail("final: shared counter = %d, want %d", got, want)
	}
	fmt.Println("PASS")
}
