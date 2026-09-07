// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests that a plugin whose OWN text calls ABI0 assembly loads and runs
// (trendvidia/go#117). On windows/amd64 the compiler rewrites the
// g-register reload that follows such a call into a load through
// `__imp_runtime.tls_g`; under internal linking the linker used to
// wrap that into a slot holding the image's rebasing delta, so the
// reload dereferenced garbage and the process died inside plugin.Open.
//
// This host deliberately imports NO crypto, so nothing the plugin calls
// is host-shared: every existing fixture in this suite shares its
// assembly-carrying packages with the host, which is why none of them
// reached the broken path.
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
	// Opening it is most of the test: edwards25519's init runs here.
	p, err := plugin.Open("asmplugin-plugin" + pluginExt)
	if err != nil {
		fail("Open(asmplugin-plugin): %v", err)
	}
	keyLen, err := p.Lookup("KeyLen")
	if err != nil {
		fail("Lookup(KeyLen): %v", err)
	}
	if got := keyLen.(func() int)(); got != 32 {
		fail("KeyLen() = %d, want 32", got)
	}
	fmt.Println("asmplugin ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "asmplugin: "+format+"\n", args...)
	os.Exit(1)
}
