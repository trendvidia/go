// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A plugin carries its own copy of the runtime and calls it directly.
// The goroutines that run finalizers and cleanups are started lazily,
// by the first SetFinalizer or AddCleanup. One that the plugin's copy
// started was created by the plugin's copy of newproc, so its stack
// returned into the plugin's goexit, and the first collection after
// Close threw "unknown caller pc" scanning it (trendvidia/go#68).
//
// The host sets no finalizer or cleanup before the plugin's, so the
// plugin's calls are the ones that start the goroutines. That is why it
// imports neither os nor anything that does: os sets finalizers on its
// standard files when it initialises. (On Windows package plugin
// imports os, but there a plugin calls the host's runtime.) After Close
// the host collects, and has a finalizer and a cleanup of its own run.
// It prints "lazygs: done" as its last line so the harness can tell a
// fault from a finished run.
package main

import (
	"plugin"
	"runtime"
	"syscall"
	"time"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	time.AfterFunc(time.Minute, func() { fail("timed out") })

	p, err := plugin.Open("lazygs-plugin" + pluginExt)
	if err != nil {
		fail("Open: " + err.Error())
	}
	for _, name := range []string{"Finalize", "Cleanup"} {
		s, err := p.Lookup(name)
		if err != nil {
			fail("Lookup(" + name + "): " + err.Error())
		}
		if !s.(func() bool)() {
			fail("the plugin's " + name + " did not run its function")
		}
	}
	if err := p.Close(); err != nil {
		fail("Close: " + err.Error())
	}

	for range 10 {
		runtime.GC()
	}
	fin := make(chan struct{})
	runtime.SetFinalizer(new([64]byte), func(*[64]byte) { close(fin) })
	cl := make(chan struct{})
	runtime.AddCleanup(new([64]byte), func(c chan struct{}) { close(c) }, cl)
	for _, done := range []chan struct{}{fin, cl} {
		for ran := false; !ran; {
			runtime.GC()
			select {
			case <-done:
				ran = true
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	syscall.Write(syscall.Stdout, []byte("lazygs: done\n"))
}

// fail panics: the host has no os.Exit, and a panic exits non-zero.
func fail(msg string) {
	panic("lazygs: " + msg)
}
