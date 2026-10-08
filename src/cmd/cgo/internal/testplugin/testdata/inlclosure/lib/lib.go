// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lib

// Desc's closure is copied into each caller that inlines Desc. Storing
// the captured constant in an interface lets the compiler fold the
// conversion into each copy, so the copies get different bodies that
// no longer read their context. Every copy reaches the linker and the
// runtime as lib.Desc.func1.
func Desc(d string) func(m map[string]any) {
	return func(m map[string]any) { m["d"] = d }
}

// Apply keeps the closure call out of line, so the copy's code pointer
// is what reaches it.
//
//go:noinline
func Apply(f func(m map[string]any)) string {
	m := map[string]any{}
	f(m)
	return m["d"].(string)
}
