// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A plugin carries its own copy of the runtime and calls it directly.
// A goroutine preempted at a prologue in plugin code used to enter the
// plugin's copy of the scheduler, through its newstack, and the thread
// stayed there: it slept in the plugin's stopm and faulted when it woke
// after Close had unmapped the image (trendvidia/go#79).
//
// Each cycle opens the plugin, runs Spin on every P while another
// goroutine forces collections (each stack scan preempts the spinning
// goroutines), waits until every goroutine is back in host code, closes
// the plugin, and then wakes every thread. Before the fix about one
// cycle in five or eight left a thread in the plugin's scheduler, so 40
// cycles all but always faulted. It prints "schedcopy: done" as its last
// line so the harness can tell a fault from a finished run.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
	"sync"
	"time"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func busy(d time.Duration) {
	for t := time.Now(); time.Since(t) < d; {
	}
}

func cycle(procs int) {
	runtime.GOMAXPROCS(procs)
	p, err := plugin.Open("schedcopy-plugin" + pluginExt)
	if err != nil {
		fail("Open: %v", err)
	}
	s, err := p.Lookup("Spin")
	if err != nil {
		fail("Lookup(Spin): %v", err)
	}
	spin := s.(func(int) int)

	stop := make(chan struct{})
	var gcs sync.WaitGroup
	gcs.Add(1)
	go func() {
		defer gcs.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	var w sync.WaitGroup
	for range procs {
		w.Add(1)
		go func() { defer w.Done(); spin(20_000_000) }()
	}
	w.Wait()
	close(stop)
	gcs.Wait()

	if err := p.Close(); err != nil {
		fail("Close: %v", err)
	}

	// Wake every thread: more Ps than threads, all busy.
	runtime.GOMAXPROCS(64)
	var b sync.WaitGroup
	for range 64 {
		b.Add(1)
		go func() { defer b.Done(); busy(2 * time.Millisecond) }()
	}
	b.Wait()
}

func main() {
	for range 40 {
		cycle(4)
	}
	fmt.Println("schedcopy: done")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "schedcopy: "+format+"\n", args...)
	os.Exit(1)
}
