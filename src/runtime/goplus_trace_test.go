// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"strings"
	"testing"
)

// TestGoplusLayoutTraceAllocFree runs execution traces with the
// alloc/free experiment while goplus.Layout types are created and freed.
// The trace used to keep those types in its type table and read them
// after the collector had freed them (trendvidia/go#63).
func TestGoplusLayoutTraceAllocFree(t *testing.T) {
	got := runTestProg(t, "testprog", "GoplusLayoutTraceAllocFree", "GODEBUG=traceallocfree=1")
	if !strings.Contains(got, "TRACE-DONE") {
		t.Fatalf("want TRACE-DONE, got:\n%s", got)
	}
}
