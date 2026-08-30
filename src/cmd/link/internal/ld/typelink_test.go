// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import "testing"

// TestSkipStaticCanonContainer pins the container exclusion list of the
// staticCanonWords scan (trendvidia/go#25). The scan is Windows-plugin
// only, so no fixture on any other platform reaches it; this exercises
// the predicate directly.
//
// The case that matters is the last group: a blanket "go." prefix — the
// shape the exclusion list started out with — also matches every package
// hosted under a go.* module domain, which would silently drop the very
// static words the scan exists to canonicalize.
func TestSkipStaticCanonContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip bool
	}{
		// Descriptor / compiler-synthesized symbol families.
		{"type:main.T", true},
		{"type:.namedata.Foo-", true},
		{"go:itab.main.T,main.I", true},
		{"go:string.\"hello\"", true},
		// -typetab slot families and load-time carriers.
		{"go.typeptr.main.T", true},
		{"go.itabptr.main.T,main.I", true},
		{"go.funcptr.main.F", true},
		{"go.typeptr.go.uber.org/zap.Logger", true},
		{"runtime.typeptrs", true},
		{"runtime.udaddrwords", true},
		{"internal/abi.Kind", true},
		{"__imp_example.com/m/pkg.Global", true},

		// Ordinary containers the scan must look inside.
		{"main.StaticErr", false},
		{"example.com/m/pkg.Table", false},
		{"main..dict.boxAny[main.T]", false},
		{"net/http.statusText", false},

		// Packages on go.* module domains: NOT slot symbols. A blanket
		// "go." prefix would exclude every one of these.
		{"go.uber.org/zap.Global", false},
		{"go.opentelemetry.io/otel.ErrHandler", false},
		{"go.etcd.io/bbolt.DefaultOptions", false},
		{"go.mongodb.org/mongo-driver/bson.ErrNilReader", false},
		{"go.opencensus.io/trace.defaultSampler", false},
		// A user package literally named "go.typeptr" would collide,
		// but a module path needs a dot-bearing domain and a slash;
		// this one is a package, not a slot, and must be scanned.
		{"go.typeptrs.io/x.Global", false},
	} {
		if got := skipStaticCanonContainer(tc.name); got != tc.skip {
			t.Errorf("skipStaticCanonContainer(%q) = %v, want %v", tc.name, got, tc.skip)
		}
	}
}
