// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "runtime"

// Collect runs a collection from the plugin's code. The plugin's copy of
// runtime.GC is called directly, so it is the plugin's gcStart that
// finds no mark workers and starts them.
func Collect() { runtime.GC() }

func main() {}
