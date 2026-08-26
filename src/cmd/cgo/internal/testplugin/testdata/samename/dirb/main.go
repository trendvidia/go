// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// dirb is the other half of the same-base-name pair: identical shape
// to dira, different answer. Both are installed as p.so/p.dll in
// sibling directories, so only the directory distinguishes them.
package main

// Name identifies which same-named plugin answered.
func Name() string { return "B" }
