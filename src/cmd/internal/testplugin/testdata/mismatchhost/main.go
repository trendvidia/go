// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Host that opens a plugin built against an alt-version of the
// common package. plugin.Open must reject it with a "different
// version" error (Bug A pkghashes guard). Anything else is a
// regression.
package main

import (
	"fmt"
	"log"
	"plugin"
	"runtime"
	"strings"

	_ "testplugin/common"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func main() {
	_, err := plugin.Open("p-mismatch" + pluginExt)
	if err == nil {
		log.Fatal("plugin.Open(p-mismatch): expected error, got nil (pkghashes guard not triggered)")
	}
	if !strings.Contains(err.Error(), "different version") {
		log.Fatalf("plugin.Open(p-mismatch): error %q does not mention \"different version\"", err)
	}
	fmt.Println("PASS")
}
