// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Version 1 of an upgradable extension. The host caches the address of
// Threshold, the parameter it tunes.
package main

// Threshold is the tunable the host takes a pointer to.
var Threshold int = 1

// Budget is unrelated state, laid out after Threshold.
var Budget int = 1000

// Report lets the host read both fields through the CURRENT module.
func Report() (int, int) { return Threshold, Budget }

func main() {}
