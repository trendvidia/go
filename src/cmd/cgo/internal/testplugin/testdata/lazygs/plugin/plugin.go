// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"runtime"
	"time"
)

// Finalize and Cleanup each attach a function to an object they drop,
// and return once it has run, or false after 10 seconds. They call the
// plugin's copies of runtime.SetFinalizer and runtime.AddCleanup, so it
// is the plugin's copy that finds no goroutine to run them and starts
// one.
func Finalize() bool {
	done := make(chan struct{})
	runtime.SetFinalizer(new([64]byte), func(*[64]byte) { close(done) })
	return wait(done)
}

func Cleanup() bool {
	done := make(chan struct{})
	runtime.AddCleanup(new([64]byte), func(c chan struct{}) { close(c) }, done)
	return wait(done)
}

func wait(done chan struct{}) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		runtime.GC()
		select {
		case <-done:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

func main() {}
