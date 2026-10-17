// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"goplus"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"
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

// TestTraceAllocFreeTypes checks which types the alloc/free trace keeps:
// a goplus.Layout type can be freed, so its objects are recorded with no
// type; a static type and a type reflect makes are never freed, and keep
// theirs.
func TestTraceAllocFreeTypes(t *testing.T) {
	const ptrSize = unsafe.Sizeof(uintptr(0))
	// 1KB each, so the type is recorded with the object (a malloc header).
	static := unsafe.Pointer(new([1024 / ptrSize]*int))
	refl := reflect.New(reflect.StructOf([]reflect.StructField{
		{Name: "P", Type: reflect.TypeFor[*int]()},
		{Name: "Pad", Type: reflect.ArrayOf(int(1024-ptrSize), reflect.TypeFor[byte]())},
	})).UnsafePointer()
	layout := goplus.NewLayout(1024, ptrSize, []byte{1}).New()
	for _, tt := range []struct {
		name string
		p    unsafe.Pointer
		want bool
	}{
		{"static type", static, true},
		{"reflect.StructOf type", refl, true},
		{"goplus.Layout type", layout, false},
	} {
		if got := runtime.TraceRecordsTypeOf(tt.p); got != tt.want {
			t.Errorf("%s: trace records a type = %v, want %v", tt.name, got, tt.want)
		}
	}
	runtime.KeepAlive(static)
	runtime.KeepAlive(refl)
	runtime.KeepAlive(layout)
}
