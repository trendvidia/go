// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

//go:noinline
func leaf(x int) int { return x*31 + 7 }

// step is not a leaf, so its prologue checks the stack guard: a
// goroutine preempted there enters the plugin's own copy of newstack.
//
//go:noinline
func step(x int) int { return leaf(x) + 1 }

// Spin calls step n times.
func Spin(n int) int {
	x := 0
	for i := 0; i < n; i++ {
		x = step(x)
	}
	return x
}

func main() {}
