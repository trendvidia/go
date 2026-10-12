// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import "testing"

// TestStdcallDecoration pins the boundary between the two meanings a
// '%' can have in a PE symbol name (trendvidia/go-archive#116).
//
// initdynimport reads the `%n` stdcall decoration off every reachable
// SDYNIMPORT extname, and does two irreversible things with what it
// finds: it multiplies the number by the pointer size into the
// `_name@N` form windows/386 external links need, and it TRUNCATES the
// extname at the '%'. Upstream that was safe because only C identifiers
// from //go:cgo_import_dynamic reached SDYNIMPORT. In this fork a
// Windows plugin converts host-shared Go symbols to SDYNIMPORT
// (peLoadImportLibrary), and objabi.PathToPrefix escapes '.' after the
// final slash as %2e — so every symbol of a package whose import path
// ends in a dotted element arrives here carrying a '%'. Reading it as a
// decoration failed the link ("failed to parse stdcall decoration") and
// truncated `gopkg.in/yaml%2ev2.Set` to `gopkg.in/yaml`.
//
// The escname fixture in cmd/cgo/internal/testplugin is the end-to-end
// half of this pin; it can only fail on the Windows legs.
func TestStdcallDecoration(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int // index of the '%', or -1 for "no decoration"
	}{
		// Real decorations: `%` + decimal digits, at the end.
		{"runtime._GetSystemTimeAsFileTime%1", 32},
		{"runtime.stdcall%0", 15},
		{"_CreateFileW%7", 12},
		{"x%12", 1},

		// PathToPrefix escapes. Dots after the final slash are escaped,
		// so these are the ordinary spellings of ordinary import paths.
		{"gopkg.in/yaml%2ev2.Set", -1},
		{"gopkg.in/yaml%2ev2.DefaultMapType", -1},
		{"example%2ecom.Counter", -1},             // root of a dotted module path (#110)
		{"go%2eexample%2eorg.Counter", -1},        // two escapes
		{"go.example.org/fakemod/lib%2ev2.X", -1}, // escape in a slashed path
		{"gopkg.in/check%2ev1.(*C).Assert", -1},   // method name after the escape
		{"example%2ecom.Fn·f", -1},                // funcval symbol

		// Unescaped names, which is nearly all of them.
		{"testplugin/common.X", -1},
		{"os.Args", -1},
		{"", -1},

		// A trailing bare '%' is not a decoration: there are no digits
		// to parse, and the old code fed Atoi an empty string.
		{"weird%", -1},
		{"gopkg.in/yaml%2ev2.X%", -1},
	} {
		if got := stdcallDecoration(tc.name); got != tc.want {
			t.Errorf("stdcallDecoration(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}
