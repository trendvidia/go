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
	msg := err.Error()
	if !strings.Contains(msg, "different version") {
		log.Fatalf("plugin.Open(p-mismatch): error %q does not mention \"different version\"", err)
	}
	// trendvidia/go#7: the loader error must name the mismatched
	// package and surface both build hashes so the failure is
	// diagnosable without decoding the raw plugin.Open message.
	if !strings.Contains(msg, "testplugin/common") {
		log.Fatalf("plugin.Open(p-mismatch): error %q does not name the mismatched package testplugin/common", err)
	}
	if !strings.Contains(msg, "build hash") {
		log.Fatalf("plugin.Open(p-mismatch): error %q does not surface the plugin/host build hashes", err)
	}
	fmt.Println("PASS")
}
