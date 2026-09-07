// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests that a plugin can bind a host-shared package whose symbol names
// carry a PathToPrefix %-escape (trendvidia/go#116).
//
// PathToPrefix escapes every '.' after the final slash, so a package
// whose import path ends in a dotted element — every gopkg.in path:
// yaml.v2, yaml.v3, check.v1, mgo.v2 — has a '%2e' in every one of its
// symbol names. On Windows the plugin converts host-shared symbols to
// SDYNIMPORT, and initdynimport used to read the first '%' in the name
// as a stdcall `%n` argument-count decoration, so the plugin link died
// with "failed to parse stdcall decoration" after truncating the
// extname at the escape. Nothing platform-specific about the fixture:
// it just has to link and share one global.
package main

import (
	"fmt"
	"os"
	"plugin"
	"runtime"

	yaml "go.example.org/fakemod/yaml.v2"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	p, err := plugin.Open("escname-plugin" + pluginExt)
	if err != nil {
		fail("Open(escname-plugin): %v", err)
	}
	setIt, err := p.Lookup("SetIt")
	if err != nil {
		fail("Lookup(SetIt): %v", err)
	}
	getIt, err := p.Lookup("GetIt")
	if err != nil {
		fail("Lookup(GetIt): %v", err)
	}

	// One instance across host and plugin: the escaped name must reach
	// the host's export table and bind through the import library
	// intact, not truncated at the '%'.
	setIt.(func(int))(42)
	if yaml.DefaultMapType != 42 {
		fail("after plugin SetIt(42), host reads yaml.DefaultMapType = %d, want 42 (escaped-name global not host-shared)", yaml.DefaultMapType)
	}
	yaml.DefaultMapType = 7
	if got := getIt.(func() int)(); got != 7 {
		fail("after host write, plugin GetIt() = %d, want 7 (escaped-name global not host-shared)", got)
	}

	fmt.Println("escname ok")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "escname: "+format+"\n", args...)
	os.Exit(1)
}
