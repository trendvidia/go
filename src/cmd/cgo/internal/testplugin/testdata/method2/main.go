// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A type can be passed to a plugin and converted to interface
// there. So its methods need to be live.

package main

import (
	"plugin"
	"runtime"

	"testplugin/method2/p"
)

var t p.T

type I interface{ M() }

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	pl, err := plugin.Open("method2" + pluginExt)
	if err != nil {
		panic(err)
	}

	f, err := pl.Lookup("F")
	if err != nil {
		panic(err)
	}

	f.(func(p.T) interface{})(t).(I).M()
}
