// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
)

// TestParseFastLeafCheck covers the values GODEBUG=fastleafcheck accepts.
// The check itself is tested in cmd/cgo/internal/testfastleaf, which has
// fastleaf C functions to run it against.
func TestParseFastLeafCheck(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"10ms", 10e6, true},
		{"1s", 1e9, true},
		{"500us", 500e3, true},
		{"500µs", 500e3, true},
		{"750ns", 750, true},
		{"", 0, false},
		{"10", 0, false},   // a unit is required
		{"ms", 0, false},   // and a number
		{"1.5s", 0, false}, // integers only
		{"-1s", 0, false},
		{"10m", 0, false},
		{"9223372036s", 9223372036e9, true},
		{"9223372037s", 0, false}, // overflows int64 nanoseconds
		{"99999999999999999999ms", 0, false},
	} {
		got, ok := runtime.ParseFastLeafCheck(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseFastLeafCheck(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
