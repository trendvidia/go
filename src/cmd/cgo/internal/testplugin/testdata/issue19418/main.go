// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

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
	p, err := plugin.Open("plugin" + pluginExt)
	if err != nil {
		panic(err)
	}

	val, err := p.Lookup("Val")
	if err != nil {
		panic(err)
	}
	got := *val.(*string)
	const want = "linkstr"
	if got != want {
		fmt.Fprintf(os.Stderr, "issue19418 value is %q, want %q\n", got, want)
		os.Exit(2)
	}
}
