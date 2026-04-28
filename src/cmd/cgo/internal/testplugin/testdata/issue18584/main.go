// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
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
	p, err := plugin.Open("plugin" + pluginExt)
	if err != nil {
		panic(err)
	}

	sym, err := p.Lookup("G")
	if err != nil {
		panic(err)
	}
	g := sym.(func() bool)
	if !g() {
		panic("expected types to match, Issue #18584")
	}
}
