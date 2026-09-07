// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// net is the point: the host does not link it, so net's init is
// PLUGIN-local and runs at plugin.Open — and net.init.0 calls
// internal/poll.InitWSA, a value that internal/poll's own init assigns.
// The host links internal/poll (through os), so that init already ran
// host-side and the plugin drops it from its schedule; unless the
// package's DATA is host-shared too, the plugin's copy of InitWSA is
// nil and the call faults (trendvidia/go#74).
//
// No network traffic: loading the plugin is what runs the init, and
// that is the test.
import "net"

// Ports exercises net without touching the network.
func Ports() string {
	return net.JoinHostPort("127.0.0.1", "8080")
}

func main() {}
