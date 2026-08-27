// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Races Plugin.HasDependents and Plugin.Dependents against
// Plugin.Close (trendvidia/go#36). Both accessors read p.pluginpath,
// which closePlugin rewrites under pluginsMu (it clones the bytes
// out of the plugin's RODATA before dlclose unmaps them). Before
// the fix the accessors read the field with no synchronisation: a
// data race under -race, and a pre-clone string header handed to the
// runtime after its bytes were unmapped. The test harness builds
// this fixture and its plugins with -race where the platform
// supports it, so the unlocked read is reported as a DATA RACE
// (exit 66) rather than passing by luck.
//
// TWO plugins are loaded, and that is load-bearing for the second
// half. runtime.findPluginModule compares the snapshotted path
// against every active module's pluginpath, and Go's string equality
// checks length before it dereferences: with only plugin1 loaded the
// sole survivor of its Close is the host, whose pluginpath is "", so
// a dangling header is never read and only the -race report catches
// anything. plugin2's pluginpath — "testplugin/plugin2" — is exactly
// as long as plugin1's, so after plugin1 closes the comparison
// reaches memequal and dereferences whatever the accessor is
// holding. plugin2 is opened FIRST so it, not plugin1, owns the
// testplugin/common package both link: that puts the dependency edge
// on plugin1 -> plugin2 and leaves plugin1 free of dependents, so
// Close(plugin1) is allowed while plugin2 stays mapped throughout.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	// Opened first, and never closed: see the package comment.
	if _, err := plugin.Open("depsrace-plugin2" + pluginExt); err != nil {
		fail("Open(depsrace-plugin2): %v", err)
	}
	p, err := plugin.Open("depsrace-plugin1" + pluginExt)
	if err != nil {
		fail("Open(depsrace-plugin1): %v", err)
	}
	if p.HasDependents() {
		fail("HasDependents() = true before any dependent loaded")
	}
	if deps := p.Dependents(); len(deps) != 0 {
		fail("Dependents() = %v before any dependent loaded, want none", deps)
	}

	stop := make(chan struct{})
	var polls, postClose int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var has bool
				var deps []string
				if i%2 == 0 {
					has = p.HasDependents()
				} else {
					deps = p.Dependents()
				}
				if has || len(deps) != 0 {
					fail("goroutine %d: HasDependents=%v Dependents=%v; plugin1 never has dependents", i, has, deps)
				}
				atomic.AddInt64(&polls, 1)
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}
	// Keep the pollers running against the closed plugin for a
	// while so the post-Close leg (pluginpath now host-cloned, the
	// plugin image unmapped) is exercised, not only the pre-Close one.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if p.HasDependents() || len(p.Dependents()) != 0 {
			fail("post-Close accessor reported dependents")
		}
		atomic.AddInt64(&postClose, 1)
	}
	close(stop)
	wg.Wait()

	if atomic.LoadInt64(&polls) == 0 {
		fail("no accessor calls recorded; the race window was not exercised")
	}
	if atomic.LoadInt64(&postClose) == 0 {
		fail("no post-Close accessor calls recorded; the unmapped-path leg was not exercised")
	}
	if err := p.Close(); err != nil {
		fail("second Close(): %v", err)
	}
	fmt.Printf("depsrace ok (polls=%d postClose=%d)\n",
		atomic.LoadInt64(&polls), atomic.LoadInt64(&postClose))
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "depsrace: "+format+"\n", args...)
	os.Exit(1)
}
