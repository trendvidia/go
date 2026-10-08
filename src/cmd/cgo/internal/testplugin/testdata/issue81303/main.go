// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Issue 81303: a plugin has its own copies of the itabs of the host.
// The runtime added these copies to the itab table as second entries
// for the same interface/type pairs. After the table grew, a lookup
// could return the copy from the plugin. A type switch compares the
// itab with the itab of the host, so it took the default case.
//
// Each plugin imports package p, which uses go/ast, so each plugin
// adds a copy of every go/ast itab of the host. The host walks a
// syntax tree before and after each plugin loads. The three plugins
// are the same because plugin.Open loads a plugin path only once.

package main

import (
	"log"
	"plugin"
	"runtime"

	"testplugin/issue81303/p"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	p.Walk()
	for _, name := range []string{"issue81303p1" + pluginExt, "issue81303p2" + pluginExt, "issue81303p3" + pluginExt} {
		pl, err := plugin.Open(name)
		if err != nil {
			log.Fatal(err)
		}
		f, err := pl.Lookup("F")
		if err != nil {
			log.Fatal(err)
		}
		f.(func())()
		p.Walk()
	}
}
