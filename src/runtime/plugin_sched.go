// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A plugin on linux and darwin carries its own copy of the runtime, and
// its code calls that copy directly. Where such a call enters the
// scheduler, the thread stays in the plugin's copy of it, sleeps there
// when idle, and faults when it wakes after Plugin.Close has unmapped
// the image (trendvidia/go#79).
//
// A plugin's reads of runtime package variables, unlike its calls,
// bind to the host's variables. So the scheduler entry points that are
// called directly go through the variables below: in a plugin they
// hold the host's functions, and the thread runs the host's scheduler.
// mcall already works this way, because the function it is given is a
// variable read too.

package runtime

import "internal/abi"

var (
	// newstack's entries into the scheduler, for a preempted goroutine.
	schedPreemptPark = preemptPark
	schedGopreemptM  = gopreempt_m
)

// threadStartPC is where newosproc starts a new thread: mstart_stub
// where threads are created with pthread_create, mstart where they are
// created with clone, and mstart for a thread cgo creates. schedinit
// sets it, and only the host's schedinit runs. A plugin's copy of
// newosproc reads the host's value, so its threads start in the host's
// code. Zero means it was not set, and the caller uses its own copy.
var (
	threadStartPC    uintptr
	cgoThreadStartPC uintptr
)

// setThreadStartPCs is called by schedinit.
func setThreadStartPCs() {
	threadStartPC = osThreadStartPC()
	cgoThreadStartPC = abi.FuncPCABI0(mstart)
}

// threadStart returns where a new thread starts: the host's entry, or
// own if threadStartPC was not set.
func threadStart(own uintptr) uintptr {
	if pc := threadStartPC; pc != 0 {
		return pc
	}
	return own
}

// cgoThreadEntry is threadStart for a thread cgo creates.
func cgoThreadEntry(own uintptr) uintptr {
	if pc := cgoThreadStartPC; pc != 0 {
		return pc
	}
	return own
}

// schedGCStartWorkers is how gcStart starts the background mark
// workers. A plugin's copy of gcStart reads the host's value, so the
// workers are created by, and run, the host's code (trendvidia/go#68).
var schedGCStartWorkers = gcBgMarkStartWorkers
