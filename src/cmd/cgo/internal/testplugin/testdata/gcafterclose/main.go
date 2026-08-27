// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Runs a garbage collection after a plugin has genuinely been
// unloaded, with the collector left ON the whole time
// (trendvidia/go#61).
//
// Every GC cycle starts with clearpools, which calls through
// runtime.poolcleanup — a funcval registered by package sync's init.
// On Windows that runtime global is host-shared: if the plugin's
// copy of sync ran its init at Open, it overwrote the host's
// registration with a funcval in the plugin's image, and the first
// GC after Close dereferenced unmapped memory:
//
//	unexpected fault address ...
//	fatal error: fault
//	runtime.clearpools()
//
// Surviving the GC is only half the property. A fix that nilled
// runtime.poolcleanup instead of stopping the duplicate registration
// would also survive, while silently disabling sync.Pool draining for
// the rest of the process — so this fixture also proves the hook still
// RUNS after the unload, by requiring that two collections with no
// intervening Put leave the pool empty.
package main

import (
	"fmt"
	"plugin"
	"runtime"
	"sync"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

var news int

var pool = sync.Pool{New: func() interface{} {
	news++
	return new([256]byte)
}}

func churn() {
	var live []*[256]byte
	for i := 0; i < 64; i++ {
		live = append(live, pool.Get().(*[256]byte))
	}
	for _, p := range live {
		pool.Put(p)
	}
}

// requireDrained fills the pool, then runs two collections with no
// intervening Put. poolCleanup moves a pool's live items to the victim
// cache on the first cycle and drops the victim cache on the second, so
// a hook that still runs leaves the pool empty and the next Get has to
// call New. A hook that was nilled leaves all 64 items in place and Get
// finds one without allocating.
func requireDrained(when string) {
	churn()
	runtime.GC()
	runtime.GC()
	before := news
	pool.Put(pool.Get())
	if news == before {
		fail("%s: sync.Pool still had a cached item after two GCs — "+
			"runtime.poolcleanup did not run, so pool draining is disabled process-wide", when)
	}
}

func main() {
	requireDrained("before Open")

	p, err := plugin.Open("gcafterclose-plugin1" + pluginExt)
	if err != nil {
		fail("Open(gcafterclose-plugin1): %v", err)
	}
	f, err := p.Lookup("F")
	if err != nil {
		fail("Lookup(F): %v", err)
	}
	if got := f.(func() int)(); got != 3 {
		fail("F() = %d, want 3", got)
	}

	// A GC while the plugin is loaded must survive too: the hook is
	// callable at this point whichever module registered it, so a
	// fault here would be a different defect than the one after
	// Close.
	requireDrained("with the plugin loaded")

	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}

	// The GC this fixture exists for: the image is unmapped, and
	// clearpools must not call through anything that lived in it.
	// Draining must still work afterwards — the host's registration
	// has to be the one left standing, not a nilled hook.
	requireDrained("after Close")
	fmt.Println("survived GC after Close")
}

func fail(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}
