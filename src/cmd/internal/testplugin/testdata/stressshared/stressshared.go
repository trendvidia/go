// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package stressshared is shared by the stress host and all stress
// plugin variants. The host imports it too, so the host's instance
// is the canonical one: plugin loads canonicalise onto it, no
// plugin-to-plugin dependency edges are created, and Close on any
// plugin is always permitted. That makes the stress invariant exact:
// Total() equals the sum of every Add() ever made, regardless of
// which plugins made them or how often plugins were closed and
// reopened in between.
package stressshared

import "sync/atomic"

var total atomic.Int64

// Add adds n to the shared counter and returns the new value.
func Add(n int64) int64 { return total.Add(n) }

// Total returns the current counter value.
func Total() int64 { return total.Load() }
