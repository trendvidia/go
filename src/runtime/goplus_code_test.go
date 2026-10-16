// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"goplus"
	"reflect"
	"runtime"
	"testing"
)

// TestGoplusCodeNotCached pins why unregistered code addresses can be
// reused safely: pcvalue does not cache results for registered code,
// whose cache key (PC and table offset) could otherwise match new code
// registered later at the same addresses.
func TestGoplusCodeNotCached(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("goplus.RegisterCode is amd64 and arm64 only")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Registration only: this range is never executed.
	base, end := uintptr(0x10000000), uintptr(0x10000040)
	r, err := goplus.RegisterCode([]goplus.Func{{
		Name:  "never.run",
		Entry: base, End: end,
		Frame: []goplus.FrameStep{{Off: 0, Size: 0}},
		File:  "never.jit",
		Lines: []goplus.LineStep{{Off: 0, Line: 3}, {Off: 32, Line: 4}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Unregister()

	// FileLine runs pcvalue on the file and line tables, as a traceback
	// would. That the answer is right shows the lookups ran.
	if file, line := runtime.FuncForPC(base+40).FileLine(base + 40); file != "never.jit" || line != 4 {
		t.Fatalf("FileLine = %s:%d, want never.jit:4", file, line)
	}
	if runtime.PCValueCacheHolds(base, end) {
		t.Fatal("pcvalue cached a result for registered code")
	}

	// Control: the same lookup for compiled code is cached, so the check
	// above would see an entry if there were one.
	pc := reflect.ValueOf(TestGoplusCodeNotCached).Pointer() + 8
	runtime.FuncForPC(pc).FileLine(pc)
	if !runtime.PCValueCacheHolds(pc, pc+1) {
		t.Fatal("control: pcvalue did not cache a result for compiled code")
	}
}
