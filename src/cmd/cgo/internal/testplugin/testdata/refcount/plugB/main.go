// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testplugin/refcount/mylib"

// IncB bumps mylib.Counter by 2 and returns it.
func IncB() int {
	return mylib.Bump(2)
}

func main() {}
