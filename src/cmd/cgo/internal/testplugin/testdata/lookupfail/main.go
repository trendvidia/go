// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises open()'s could-not-find-symbol failure path, twice
// (trendvidia/go-archive#37). The plugin is built with
// -ldflags=-pluginpath=lookupfail.mismatch but WITHOUT the matching
// -gcflags=-p, so its exported symbols keep their compile-time
// package prefix while moduledata.pluginpath carries the override;
// open()'s symbol loop asks the dynamic loader for
// "lookupfail.mismatch.<sym>", which no platform can resolve. That
// deterministically drives the mid-load failure path on every OS
// with no binary surgery.
//
// Scope of the trick, so the next reader does not over-read the
// coverage: the same mismatch also unroots
// "lookupfail.mismatch..inittask" in the linker
// (cmd/link/internal/ld/deadcode.go), so inittaskSym finds nothing,
// moduledata.inittasks comes back empty and open()'s doInit is a
// no-op. The plugin's exported symbols survive via the go:plugin.tabs
// root, so the symbol loop still runs and still fails — but it fails
// on its FIRST iteration, on a module whose package inits never ran.
// The partly-filled variant of the failure (some symbols resolved,
// funcDescriptors non-empty, inits already run) is not covered here.
//
// The first Open must return a "could not find symbol" error. The
// path used to return with the cached placeholder's loaded channel
// never closed and err unset, so a second Open of the same path took
// the cached fast path and blocked on <-p.loaded forever; it must
// instead report the cached failure. A concurrent Open is checked
// too: a caller that reached the cached entry before the load failed
// parks on <-p.loaded, and on waking must learn the failure rather
// than receive a *Plugin with nil syms and a nil error. The watchdog
// turns the historic hang into a crisp failure long before the go
// test timeout.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"strings"
	"sync"
	"time"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

// concurrentOpens is the number of goroutines that race the very
// first Open of the path. One of them wins pluginsMu and becomes the
// loader; the rest queue on the mutex, which open holds across the
// slow dlopen/LoadLibrary and lastmoduleinit and releases exactly as
// the load-in-flight window opens — so they read the placeholder's
// still-empty err and park on <-p.loaded, which is the leg under
// test.
const concurrentOpens = 8

func main() {
	go func() {
		time.Sleep(30 * time.Second)
		fmt.Fprintln(os.Stderr, "lookupfail: watchdog fired; an Open call is hung")
		os.Exit(3)
	}()

	path := "lookupfail-plugin" + pluginExt

	// Every racer must be told the load failed: the loader by its own
	// return, the waiters by the re-read of p.err after <-p.loaded.
	// Before that re-read existed, a waiter got (p, nil) — a *Plugin
	// whose Lookups all report "not found" and whose Close no-ops,
	// with no way to learn the load never completed.
	errs := make([]error, concurrentOpens)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p, err := plugin.Open(path)
			errs[i] = err
			if err == nil && p == nil {
				fail("Open(%s) returned (nil, nil)", path)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	sawDirect := false
	for i, err := range errs {
		if err == nil {
			fail("concurrent Open #%d of %s succeeded; want could-not-find-symbol error", i, path)
		}
		// The symbol named is whichever map iteration tried first;
		// only the failure class is deterministic.
		if !strings.Contains(err.Error(), "could not find symbol") {
			fail("concurrent Open #%d of %s error = %v; want could-not-find-symbol", i, path, err)
		}
		if !strings.Contains(err.Error(), "(previous failure)") {
			sawDirect = true
		}
	}
	if !sawDirect {
		fail("no racer reported the load failure directly; the loader's own error was never observed")
	}

	// Serial re-Open: the cached entry is failed, so this is the
	// "(previous failure)" leg — and the one that used to hang.
	_, err := plugin.Open(path)
	if err == nil {
		fail("Open(%s) after the failed load succeeded; want cached failure", path)
	}
	if !strings.Contains(err.Error(), "could not find symbol") ||
		!strings.Contains(err.Error(), "(previous failure)") {
		fail("Open(%s) after the failed load error = %v; want cached could-not-find-symbol (previous failure)", path, err)
	}

	fmt.Println("lookupfail ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "lookupfail: "+format+"\n", args...)
	os.Exit(1)
}
