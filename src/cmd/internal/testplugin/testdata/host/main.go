// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Pure-Go host (no cgo) that exercises the v3 plugin runtime: opens
// two plugins, looks up a function and a variable, validates that
// both plugins share the host's common.X.
package main

import (
	"fmt"
	"log"
	"plugin"
	"runtime"

	"testplugin/common"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	if got, want := common.X, 3; got != want {
		log.Fatalf("before any plugin load: common.X=%d, want %d", got, want)
	}

	p1, err := plugin.Open("p1" + pluginExt)
	if err != nil {
		log.Fatalf("plugin.Open(p1): %v", err)
	}
	// p1.init() did common.X *= 7
	if got, want := common.X, 3*7; got != want {
		log.Fatalf("after p1: common.X=%d, want %d", got, want)
	}

	fSym, err := p1.Lookup("F")
	if err != nil {
		log.Fatalf("p1.Lookup(F): %v", err)
	}
	if got, want := fSym.(func() int)(), 3; got != want {
		log.Fatalf("p1.F()=%d, want %d", got, want)
	}

	sevenSym, err := p1.Lookup("Seven")
	if err != nil {
		log.Fatalf("p1.Lookup(Seven): %v", err)
	}
	if got, want := *sevenSym.(*int), 7; got != want {
		log.Fatalf("*p1.Seven=%d, want %d", got, want)
	}

	readXSym, err := p1.Lookup("ReadCommonX")
	if err != nil {
		log.Fatalf("p1.Lookup(ReadCommonX): %v", err)
	}
	if got, want := readXSym.(func() int)(), 3*7; got != want {
		log.Fatalf("p1.ReadCommonX()=%d, want %d (plugin not seeing host's common.X)", got, want)
	}

	p2, err := plugin.Open("p2" + pluginExt)
	if err != nil {
		log.Fatalf("plugin.Open(p2): %v", err)
	}
	// p2.init() set common.X = 2
	if got, want := common.X, 2; got != want {
		log.Fatalf("after p2: common.X=%d, want %d", got, want)
	}

	gSym, err := p2.Lookup("G")
	if err != nil {
		log.Fatalf("p2.Lookup(G): %v", err)
	}
	if got, want := gSym.(func() int)(), 11; got != want {
		log.Fatalf("p2.G()=%d, want %d", got, want)
	}

	// p1's view of common.X must reflect p2's mutation — both
	// plugins are sharing the host's single common moduledata.
	if got, want := readXSym.(func() int)(), 2; got != want {
		log.Fatalf("after p2: p1.ReadCommonX()=%d, want %d (cross-plugin shared package broken)", got, want)
	}

	fmt.Println("PASS")
}
