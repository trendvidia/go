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

// traceTypesSink keeps TestTraceAllocFreeTypes's objects in the heap.
var traceTypesSink []unsafe.Pointer

// TestTraceAllocFreeTypes checks which types the alloc/free trace keeps:
// a goplus.Layout type can be freed, so its objects are recorded with no
// type; a static type and a type reflect makes are never freed, and keep
// theirs. A dead object in a span not yet swept keeps only a static
// type, the one kind that can be told from a freed layout type without
// reading it. Objects of 1KB have a malloc header, and of 64KB a span to
// themselves, the two places a type is recorded with an object.
func TestTraceAllocFreeTypes(t *testing.T) {
	const ptrSize = unsafe.Sizeof(uintptr(0))
	reflectType := func(size uintptr) reflect.Type {
		return reflect.StructOf([]reflect.StructField{
			{Name: "P", Type: reflect.TypeFor[*int]()},
			{Name: "Pad", Type: reflect.ArrayOf(int(size-ptrSize), reflect.TypeFor[byte]())},
		})
	}
	for _, tt := range []struct {
		name       string
		p          unsafe.Pointer
		live, dead bool // whether the trace records a type
	}{
		{"static type, 1KB", unsafe.Pointer(new([1024 / ptrSize]*int)), true, true},
		{"static type, 64KB", unsafe.Pointer(new([64 << 10 / ptrSize]*int)), true, true},
		{"reflect.StructOf type, 1KB", reflect.New(reflectType(1024)).UnsafePointer(), true, false},
		{"reflect.StructOf type, 64KB", reflect.New(reflectType(64 << 10)).UnsafePointer(), true, false},
		{"goplus.Layout type, 1KB", goplus.NewLayout(1024, ptrSize, []byte{1}).New(), false, false},
		{"goplus.Layout type, 64KB", goplus.NewLayout(64<<10, ptrSize, []byte{1}).New(), false, false},
	} {
		traceTypesSink = append(traceTypesSink, tt.p)
		live, dead, gc := runtime.TraceTypesOf(tt.p)
		if gc == nil {
			t.Errorf("%s: the collector reads no type", tt.name)
			continue
		}
		for _, c := range []struct {
			state string
			got   unsafe.Pointer
			want  bool
		}{{"live", live, tt.live}, {"dead", dead, tt.dead}} {
			switch {
			case c.want && c.got != gc:
				t.Errorf("%s: trace records type %p for a %s object, the collector reads %p", tt.name, c.got, c.state, gc)
			case !c.want && c.got != nil:
				t.Errorf("%s: trace records type %p for a %s object, want none", tt.name, c.got, c.state)
			}
		}
	}
	traceTypesSink = nil
}
