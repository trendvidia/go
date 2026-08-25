// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Races concurrent Plugin.Lookup calls against Plugin.Close
// (trendvidia/go#23). Before the fix, lookup read p.syms and
// p.pluginpath without pluginsMu while closePlugin mutated both
// under it — a data race detectable by -race, and in principle a
// torn map read or a dangling pluginpath string. The fixture
// hammers Lookup from several goroutines while the main goroutine
// closes the plugin, then validates every outcome:
//
//   - every recorded result is either a success or a "not found"
//     error (never a crash — reaching this point proves that);
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
	var okCount, errCount int64
	var mu sync.Mutex // guards samples
	var samples []plugin.Symbol

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sym, err := p.Lookup("F")
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					continue
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

	fmt.Printf("lookuprace ok (ok=%d err=%d samples=%d)\n",
		atomic.LoadInt64(&okCount), atomic.LoadInt64(&errCount), len(samples))
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "lookuprace: "+format+"\n", args...)
	os.Exit(1)
}
