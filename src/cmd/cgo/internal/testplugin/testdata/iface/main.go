// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"plugin"
	"runtime"

	"testplugin/iface_i"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	a, err := plugin.Open("iface_a" + pluginExt)
	if err != nil {
		log.Fatalf(`plugin.Open("iface_a%s"): %v`, pluginExt, err)
	}
	b, err := plugin.Open("iface_b" + pluginExt)
	if err != nil {
		log.Fatalf(`plugin.Open("iface_b%s"): %v`, pluginExt, err)
	}

	af, err := a.Lookup("F")
	if err != nil {
		log.Fatalf(`a.Lookup("F") failed: %v`, err)
	}
	bf, err := b.Lookup("F")
	if err != nil {
		log.Fatalf(`b.Lookup("F") failed: %v`, err)
	}
	if af.(func() interface{})() != bf.(func() interface{})() {
		panic("empty interfaces not equal")
	}

	ag, err := a.Lookup("G")
	if err != nil {
		log.Fatalf(`a.Lookup("G") failed: %v`, err)
	}
	bg, err := b.Lookup("G")
	if err != nil {
		log.Fatalf(`b.Lookup("G") failed: %v`, err)
	}
	if ag.(func() iface_i.I)() != bg.(func() iface_i.I)() {
		panic("nonempty interfaces not equal")
	}
}
