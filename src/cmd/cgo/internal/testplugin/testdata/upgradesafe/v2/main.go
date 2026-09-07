// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Version 2 of the same extension: different behaviour, different
// layout, same ABI.
package main

var tuning = [64]int{}

func Score(sample string) int { return len(sample)*10 + tuning[0] }

func Version() int { return 2 }

func main() {}
