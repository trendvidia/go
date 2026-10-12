// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The canonicaldata plugin embeds type descriptors, itabs, and data
// addresses in static data through each of trendvidia/go-archive#25's edges,
// then hands the resulting values to the host, which checks them
// against its own canonical copies.
package main

import "testplugin/canonicaldata/shared"

// Edge 1: generic dictionaries. boxAny and boxI are instantiated only
// in this module, so their dictionaries are plugin data; the rtype and
// itab words inside them are what the host-side assertions observe.
// noinline: an inlined instantiation performs the conversion at the
// call site from slot-routed descriptors and never materializes the
// dictionary this edge is about.
//
//go:noinline
func boxAny[T any](v T) any { return v }

//go:noinline
func boxI[T shared.I](v T) shared.I { return v }

func MakeAny() any { return boxAny(shared.Concrete{V: 7}) }

func MakeI() shared.I { return boxI(shared.Concrete{V: 8}) }

// Edge 2: statically-initialized interface globals. The initializers
// below are laid down by the compiler as itab and rtype words in this
// module's data section, with no conversion executing at run time.
var StaticErr error = shared.MyErr{Code: 3}

var StaticAny any = shared.Concrete{V: 9}

func GetStaticErr() error { return StaticErr }

func GetStaticAny() any { return StaticAny }

// Edge 3: type-assert descriptors. The v.(shared.I) below compiles to
// a descriptor whose interface-type word is this module's shared.I.
func AsI(v any) (shared.I, bool) {
	i, ok := v.(shared.I)
	return i, ok
}

// Edge 4: addresses of cross-module globals embedded in static data,
// with and without an addend.
var GlobalPtr = &shared.Global

var Arr2Ptr = &shared.Arr[2]

func PtrGlobal() *int { return GlobalPtr }

func PtrArr2() *int { return Arr2Ptr }

// ReadGlobal observes shared.Global through this module's code path
// (slot- or import-routed), for the one-logical-variable check.
func ReadGlobal() int { return shared.Global }

// Edge 4b: a static func VALUE of a host-shared function. This is the
// one shape in which the scan meets a TEXT symbol: the compiler emits
// the funcval container shared.Fn·f into this module, holding a
// single R_ADDR to shared.Fn — whose body the host owns. The container
// itself is ordinary user-package data, so the scan looks inside it.
//
// CallFn only exercises the call; the func value's ADDRESS identity
// across the two modules is trendvidia/go-archive#106 and is deliberately not
// asserted here.
var FnPtr = shared.Fn

func CallFn() int { return FnPtr() }

func main() {}
