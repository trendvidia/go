// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package rootorg sits at the root of module example.org, so its
// symbols are named `example%2eorg.X`. It is imported by the rootmod
// host AND by plugin plugA, so on Windows the plugin's copy of Value
// must be host-shared through the import library — the other half of
// the trendvidia/go-archive#110 surface, exercising peSkipSymName rather than
// the canonicalisation registry.
package rootorg

// Value is the host-shared global.
var Value int
