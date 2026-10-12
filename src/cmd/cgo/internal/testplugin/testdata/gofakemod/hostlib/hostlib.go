// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package hostlib is imported by the godomain host AND plugin plugA,
// so on Windows the plugin's copy of Value must be host-shared through
// the import library. Its go.*-domain import path pins that the
// slot-family filter in peSkipSymName matches only the three
// go.<kind>. families, never the bare `go.` prefix — a blanket filter
// would drop Value from the host's export table and leave the plugin
// its own instance (trendvidia/go-archive#108).
package hostlib

// Value is the host-shared global.
var Value int
