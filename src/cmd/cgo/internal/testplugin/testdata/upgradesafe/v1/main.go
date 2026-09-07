// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Version 1 of an extension written to the upgradable contract: it
// exposes behaviour through exported FUNCTIONS, and every value that
// crosses the boundary has a type the host already knows.
package main

// Score is the whole ABI. It takes and returns predeclared types, so
// nothing the host holds afterwards points into this image.
func Score(sample string) int { return len(sample) }

// Version lets the host confirm which build answered.
func Version() int { return 1 }

func main() {}
