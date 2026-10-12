// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests that a package whose init task is host-shared also has its data
// host-shared, so a plugin never reads a global the host's init was the
// only one to assign (trendvidia/go-archive#74).
//
// The host links internal/poll (through os) but NOT net. So
// internal/poll's init task is published to the plugin and dropped from
// the plugin's schedule, while net's init stays plugin-local and runs
// at Open — and it calls internal/poll.InitWSA, a sync.OnceFunc value
// that internal/poll's init assigns. With the package's data left
// plugin-local, that call reads nil: measured on windows/{amd64,arm64}
// as 0xc0000005 at net/fd_windows.go:27 inside plugin.Open.
//
// Re-running internal/poll's init plugin-side is NOT the alternative
// fix: its task's PCs are host-shared text compiled without -dynlink,
// so it would initialise the host's copy a second time.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	// Opening it is most of the test: net's init runs here.
	p, err := plugin.Open("internalinit-plugin" + pluginExt)
	if err != nil {
		fail("Open(internalinit-plugin): %v", err)
	}
	ports, err := p.Lookup("Ports")
	if err != nil {
		fail("Lookup(Ports): %v", err)
	}
	if got := ports.(func() string)(); got != "127.0.0.1:8080" {
		fail("Ports() = %q, want %q", got, "127.0.0.1:8080")
	}
	// Touch the host's own internal/poll user so the host keeps it
	// linked no matter how the fixture is edited later.
	if _, err := os.Stat(os.Args[0]); err != nil {
		fail("Stat(%s): %v", os.Args[0], err)
	}
	fmt.Println("internalinit ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "internalinit: "+format+"\n", args...)
	os.Exit(1)
}
