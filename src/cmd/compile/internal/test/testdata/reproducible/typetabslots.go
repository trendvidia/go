// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typetabslots

// Interface-heavy fixture compiled by TestTypetabSlotsReproducible and
// TestTypetabSlotsPresent: under -typetab each type-descriptor / itab
// reference below routes through a go.typeptr./go.itabptr. slot that is
// recorded during parallel compilation and emitted once at dumpdata
// time.
//
// TestTypetabSlotsPresent names the exact slots this file must produce
// (typetabSlotsWant in ../../typetab_test.go), including which compiler
// path records each one, so editing the types or conversions here means
// updating that list too.

type Counter struct{ n int }

func (c *Counter) Inc() int { c.n++; return c.n }

type Increments interface {
	Inc() int
}

func assertAny(v any) (Increments, bool) {
	i, ok := v.(Increments)
	return i, ok
}

func wrap(c *Counter) any {
	var i Increments = c
	return i
}

func round(c *Counter) int {
	if i, ok := assertAny(wrap(c)); ok {
		return i.Inc()
	}
	return -1
}
