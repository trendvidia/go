// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Drives trendvidia/go-archive#62: a cgo plugin's runtime-init handshake.
//
// The plugin is built with -extldflags=-Wl,--exclude-all-symbols, which
// makes the .def authoritative and so removes every export the linker
// did not name on purpose. Before the fix that stripped
// x_cgo_notify_runtime_init_done — present on mingw only because ld
// auto-exports what it can — and plugin.Open's GetProcAddress for it
// quietly found nothing. The load still succeeded, Go->C calls still
// worked, and the first C->Go callback blocked forever in
// _cgo_wait_runtime_init_done with nothing logged. On windows/arm64,
// which links plugins internally, the export was absent from every cgo
// plugin.
//
// The linker now names the symbol itself, so the export is a decision
// and not a side effect. This checks both halves of that: the export is
// in the table, and the callback it enables returns.
//
// A regression here HANGS rather than failing, so the test runs it under
// a deadline.
package main

import (
	"fmt"
	"os"
	"plugin"
	"syscall"
)

func main() {
	p, err := plugin.Open("cgonotify-plugin.dll")
	if err != nil {
		fail("Open: %v", err)
	}

	// The export itself, read straight from the loaded image. Checking
	// only the callback below would leave "it worked" and "the handshake
	// was never needed here" indistinguishable, which is exactly the
	// ambiguity that let #62 sit unnoticed on windows/arm64.
	h, err := syscall.LoadLibrary("cgonotify-plugin.dll")
	if err != nil {
		fail("LoadLibrary: %v", err)
	}
	defer syscall.FreeLibrary(h)
	if _, err := syscall.GetProcAddress(h, "x_cgo_notify_runtime_init_done"); err != nil {
		fail("the plugin does not export x_cgo_notify_runtime_init_done: %v\n"+
			"Its cgo runtime-init handshake can never complete, so any callback from C into "+
			"the plugin's Go code blocks forever.", err)
	}

	goToC, err := p.Lookup("GoToC")
	if err != nil {
		fail("Lookup GoToC: %v", err)
	}
	if got := goToC.(func() int)(); got != 7 {
		fail("GoToC() = %d, want 7", got)
	}

	// The guarded direction. Before the fix this is where the process
	// stopped responding.
	cToGo, err := p.Lookup("CToGo")
	if err != nil {
		fail("Lookup CToGo: %v", err)
	}
	if got := cToGo.(func() int)(); got != 42 {
		fail("CToGo() = %d, want 42", got)
	}

	fmt.Println("ok")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
