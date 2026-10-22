// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// An interface call reaches a method whose signature differs from the
// call's only in struct tags, through a pointer conversion that ignores
// tags (trendvidia/go#85). S.M (the method has the tag) and S.O (the
// call has it) must be kept. S.N's only call differs in a field name,
// which the conversion does not ignore, so S.N is still dead. S.L's
// only call names a different type with the same type string, main.T,
// and neither side has a tag, so S.L is still dead too.

package main

type P = struct {
	X int `json:"x"`
}
type Q = struct{ X int }
type R = struct{ Y int }

type T int

type S struct{}

func (S) M(p P) P { return p }
func (S) N(p P) P { return p }
func (S) O(q Q) Q { return q }
func (S) L(t T)   {}

type IM interface{ M(P) P }
type IN interface{ N(P) P }
type IO interface{ O(Q) Q }
type IL interface{ L(T) }

func main() {
	var m IM = S{}
	(*(*interface{ M(Q) Q })(&m)).M(Q{})

	var o IO = S{}
	(*(*interface{ O(P) P })(&o)).O(P{})

	var n IN = S{}
	_ = n
	var r interface{ N(R) R }
	if r != nil {
		r.N(R{})
	}

	var l IL = S{}
	_ = l
	type T int
	var lt interface{ L(T) }
	if lt != nil {
		lt.L(0)
	}
}
