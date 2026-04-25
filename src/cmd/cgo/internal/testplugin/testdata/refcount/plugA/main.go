// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testplugin/refcount/mylib"

// IncA bumps mylib.Counter by 1 and returns it.
func IncA() int {
	return mylib.Bump(1)
}

func main() {}
