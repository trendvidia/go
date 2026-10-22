// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A plugin carries its own copy of the runtime and calls it directly.
// The garbage collector's background mark workers are started lazily,
// by the first collection, and a collection the plugin started used to
// start them with the plugin's code: they ran in the image, and the
// next collection after Close faulted (trendvidia/go#68).
//
// The host makes sure no collection has run before the plugin's, so the
// plugin's is the one that starts the workers. It then closes the
// plugin and collects again. Run with the argument "maxprocs", the host
// instead starts one worker itself, at GOMAXPROCS 1, and raises
// GOMAXPROCS before the plugin's collection, which then has to start
// the rest. It prints "gcworkers: done" as its last line so the harness
// can tell a fault from a finished run.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"runtime/debug"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func numGC() uint32 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.NumGC
}

func main() {
	// No collection may start before the plugin's. Explicit runtime.GC
	// calls still run with the collector off.
	debug.SetGCPercent(-1)
	if n := numGC(); n != 0 {
		fail("%d collections ran before the plugin's; the host started the workers", n)
	}
	var want uint32 = 1
	if len(os.Args) > 1 && os.Args[1] == "maxprocs" {
		// Workers are never stopped, so the host's collection at
		// GOMAXPROCS 1 leaves exactly one, and the plugin's starts the
		// other three.
		runtime.GOMAXPROCS(1)
		runtime.GC()
		runtime.GOMAXPROCS(4)
		want = 2
	}

	p, err := plugin.Open("gcworkers-plugin" + pluginExt)
	if err != nil {
		fail("Open: %v", err)
	}
	s, err := p.Lookup("Collect")
	if err != nil {
		fail("Lookup(Collect): %v", err)
	}
	s.(func())()
	if n := numGC(); n != want {
		fail("after the plugin's collection NumGC = %d, want %d", n, want)
	}

	if err := p.Close(); err != nil {
		fail("Close: %v", err)
	}
	for range 10 {
		runtime.GC()
	}
	fmt.Println("gcworkers: done")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gcworkers: "+format+"\n", args...)
	os.Exit(1)
}
