// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Races concurrent Plugin.Lookup calls against Plugin.Close
// (trendvidia/go-archive#23). Before the fix, lookup read p.syms and
// p.pluginpath without pluginsMu while closePlugin mutated both
// under it — a data race detectable by -race, and in principle a
// torn map read or a dangling pluginpath string. The fixture
// hammers Lookup from several goroutines while the main goroutine
// closes the plugin, then validates every outcome:
//
//   - the racing goroutines alternate a symbol that exists ("F")
//     with one that never does ("NoSuchSymbol"), so lookup's error
//     branch — the read of p.pluginpath, whose plugin-RODATA
//     backing is the dangling-string hazard — runs against the
//     live plugin on every other call, not only after Close;
//   - every recorded result is either a success, a "not found"
//     error of the documented shape, or — after Close — a "plugin
//     is closed" error (never a crash — reaching this point proves
//     that);
//   - each racing goroutine keeps looking up until it has itself
//     seen "F" fail — which only Close can cause — so the
//     post-Close leg is exercised by every goroutine regardless of
//     scheduling, and errCount > 0 is asserted, not assumed. That
//     wait is bounded by a deadline: a regression that leaves
//     p.syms populated after Close never makes "F" fail, and
//     without the bound the workers would spin forever and the
//     fixture would hang instead of reporting postCloseCount == 0;
//   - function values handed out before Close still panic with the
//     recoverable stub message when called afterwards;
//   - a second Close is an idempotent no-op.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"strings"
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
	// Backstop for a hang anywhere outside the worker loop's own
	// deadline — a Lookup or a Close that never returns.
	go func() {
		time.Sleep(60 * time.Second)
		fmt.Fprintln(os.Stderr, "lookuprace: watchdog fired; a Lookup or Close is hung")
		os.Exit(3)
	}()

	p, err := plugin.Open("lookuprace-plugin1" + pluginExt)
	if err != nil {
		fail("Open(lookuprace-plugin1): %v", err)
	}

	// Sanity-check the symbol before racing anything.
	if fn, err := p.Lookup("F"); err != nil {
		fail("Lookup(F) before race: %v", err)
	} else if got := fn.(func() int)(); got != 3 {
		fail("F() = %d, want 3", got)
	}

	stop := make(chan struct{})
	var okCount, errCount, postCloseCount int64
	var mu sync.Mutex // guards samples
	var samples []plugin.Symbol

	// Bound on how long a worker will hold out for its own post-Close
	// failure. Reaching it means Close did not make "F" stop
	// resolving — exactly the regression this fixture exists to
	// catch — so the workers must give up and let main report
	// postCloseCount == 0 rather than spin until the suite times out.
	deadline := time.Now().Add(15 * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sawPostClose := false
			for i := 0; ; i++ {
				// Honour stop only after this goroutine has itself
				// observed a post-Close lookup failure: "F" resolves
				// for as long as the plugin is live, so an error on
				// it can only mean Close won. Without this, main's
				// close(stop) — fired the instant Close returns —
				// can end the loop while every recorded result is
				// still pre-Close, and the post-Close leg goes
				// untested on a loaded machine. The deadline is the
				// escape hatch that keeps main's postCloseCount
				// assertion reachable when "F" never fails.
				if sawPostClose || time.Now().After(deadline) {
					select {
					case <-stop:
						return
					default:
					}
				}
				symName := "F"
				if i%2 == 1 {
					// A name that never resolves: drives lookup's
					// error branch — and its read of the
					// plugin-RODATA-backed p.pluginpath — while the
					// plugin is still live, not only after Close.
					symName = "NoSuchSymbol"
				}
				sym, err := p.Lookup(symName)
				if err != nil {
					// Two documented shapes: "not found" while the
					// plugin is live, "plugin is closed" once Close
					// has retired it (trendvidia/go-archive#64). "F" can only
					// fail the second way, so that is the post-Close
					// signal; NoSuchSymbol may fail either way.
					closed := strings.Contains(err.Error(), "plugin is closed")
					notFound := strings.Contains(err.Error(), "symbol "+symName+" not found in plugin")
					if !closed && (symName == "F" || !notFound) {
						fail("Lookup(%s) error has unexpected shape: %v", symName, err)
					}
					atomic.AddInt64(&errCount, 1)
					if symName == "F" {
						sawPostClose = true
						atomic.AddInt64(&postCloseCount, 1)
					}
					continue
				}
				if symName == "NoSuchSymbol" {
					fail("Lookup(NoSuchSymbol) unexpectedly succeeded")
				}
				atomic.AddInt64(&okCount, 1)
				mu.Lock()
				if len(samples) < 32 {
					samples = append(samples, sym)
				}
				mu.Unlock()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}
	close(stop)
	wg.Wait()

	if atomic.LoadInt64(&okCount) == 0 {
		fail("no successful Lookups recorded; the race window was not exercised")
	}
	if atomic.LoadInt64(&errCount) == 0 {
		fail("no failed Lookups recorded; the error branch was not exercised")
	}
	if atomic.LoadInt64(&postCloseCount) == 0 {
		fail("no post-Close Lookup failures recorded; the post-Close leg was not exercised")
	}

	// Every function value Lookup ever handed out must now route to
	// the runtime's closed-plugin stub: calling it panics, and the
	// panic is recoverable.
	mu.Lock()
	defer mu.Unlock()
	for i, sym := range samples {
		fn, ok := sym.(func() int)
		if !ok {
			fail("sample %d: F has unexpected type %T", i, sym)
		}
		called := false
		func() {
			defer func() {
				v := recover()
				if v == nil {
					if called {
						fail("sample %d: F() after Close returned normally; want stub panic", i)
					} else {
						fail("sample %d: F() after Close did not panic", i)
					}
				} else if !strings.Contains(fmt.Sprint(v), "invoked after Plugin.Close") {
					fail("sample %d: F() after Close panicked with %v; want stub message", i, v)
				}
			}()
			fn()
			called = true
		}()
	}

	// Idempotent: a second Close is a no-op.
	if err := p.Close(); err != nil {
		fail("second Close(): %v", err)
	}

	fmt.Printf("lookuprace ok (ok=%d err=%d postclose=%d samples=%d)\n",
		atomic.LoadInt64(&okCount), atomic.LoadInt64(&errCount),
		atomic.LoadInt64(&postCloseCount), len(samples))
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "lookuprace: "+format+"\n", args...)
	os.Exit(1)
}
