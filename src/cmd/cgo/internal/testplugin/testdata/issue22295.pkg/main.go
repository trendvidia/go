// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore

package main

import (
	"log"
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
	p, err := plugin.Open("issue.22295" + pluginExt)
	if err != nil {
		log.Fatal(err)
	}
	f, err := p.Lookup("F")
	if err != nil {
		log.Fatal(err)
	}
	const want = 2503
	got := f.(func() int)()
	if got != want {
		log.Fatalf("got %d, want %d", got, want)
	}
}
