// run

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The linker dropped a method whose only interface call uses a
// signature that differs from the method's only in struct tags, and
// the call jumped to the dropped body (trendvidia/go#85). Each method
// below is called only through such a signature, so each case keeps
// its method alive on its own.

package main

type P = struct {
	X int `json:"x"`
}
type Q = struct{ X int }

type impl struct{}

// Pointer conversion: the method has the tag, the call does not.
func (impl) M(p P) P { return P{p.X + 1} }

// Channel conversion.
func (impl) N(p P) P { return P{p.X + 2} }

// The call has the tag, the method does not.
func (impl) O(q Q) Q { return Q{q.X + 3} }

// The tag is inside another type of the signature.
func (impl) S(s []P) int { return len(s) + s[0].X }

func main() {
	var m interface{ M(P) P } = impl{}
	if got := (*(*interface{ M(Q) Q })(&m)).M(Q{10}); got.X != 11 {
		panic(got)
	}

	c := make(chan interface{ N(P) P }, 1)
	c <- impl{}
	if got := (<-(chan interface{ N(Q) Q })(c)).N(Q{10}); got.X != 12 {
		panic(got)
	}

	var o interface{ O(Q) Q } = impl{}
	if got := (*(*interface{ O(P) P })(&o)).O(P{10}); got.X != 13 {
		panic(got)
	}

	var s interface{ S([]P) int } = impl{}
	if got := (*(*interface{ S([]Q) int })(&s)).S([]Q{{10}}); got != 11 {
		panic(got)
	}
}
