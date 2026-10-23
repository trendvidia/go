// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// TestGoplusRuntimeOwned pins which callers a goplus.AllocDomain stack
// limit never refuses at, by the symbol-name shapes the toolchain emits.
func TestGoplusRuntimeOwned(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"runtime.deferreturn", true},
		{"runtime.gopanic", true},
		{"runtime.mapassign_faststr", true},
		{"internal/runtime/maps.(*Map).Clear", true},
		{"internal/runtime/maps.(*Map).putSlotSmall", true},
		{"sync.(*Mutex).Lock", true},
		{"internal/sync.(*Mutex).lockSlow", true},
		{"sync.OnceValue[go.shape.int].func1", true},
		{"type:.hash.main.key", true},
		{"type:.eq.[2]interface {}", true},
		// Implemented in the runtime, named for the package it serves.
		{"sync.runtime_Semacquire", true},
		{"internal/poll.runtime_pollWait", true},
		{"os.runtime_args", true},
		{"example.com/x.runtime_F", true},

		// Helpers compiled code calls for one stateless operation, which a
		// recursion's growth can land in at every level.
		{"runtime.checkptrAlignment", false},
		{"runtime.checkptrStraddles", false},
		{"runtime.checkptrBase", false},
		{"runtime.assertE2I", false},
		{"runtime.assertE2I2", false},
		{"runtime.typeAssert", false},
		{"runtime.interfaceSwitch", false},
		{"runtime.getitab", false},
		{"runtime.convT", false},
		{"runtime.convTstring", false},
		{"runtime.convT64", false},
		// Close to the helpers, but still runtime code.
		{"runtime.mapaccess1_faststr", true},
		{"runtime.itabAdd", true},
		{"runtime.growslice", true},

		{"fmt.(*pp).printValue", false},
		{"errors.is", false},
		{"main.main", false},
		{"main.runtime", false},
		{"main.(*T).runtime_x", false},
		{"runtimex.f", false},
		{"syncx.(*Mutex).Lock", false},
		{"example.com/runtime.F", false},
		{"example.com/sync.F", false},
	} {
		if got := runtime.GoplusRuntimeOwned(tc.name); got != tc.want {
			t.Errorf("goplusRuntimeOwned(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGoplusPanicSafeAt checks the classification from a return address,
// as newstack sees it: one inside a runtime or sync function, inside an
// ordinary function, and outside any function the runtime knows.
func TestGoplusPanicSafeAt(t *testing.T) {
	// A return address points just past a call, inside its function.
	inside := func(f any) uintptr { return reflect.ValueOf(f).Pointer() + 1 }
	for _, tc := range []struct {
		what string
		pc   uintptr
		want bool
	}{
		{"runtime.GC", inside(runtime.GC), false},
		{"sync.(*Mutex).Lock", inside((*sync.Mutex).Lock), false},
		{"test function", inside(TestGoplusPanicSafeAt), true},
		{"unknown code", 1, false},
	} {
		if got := runtime.GoplusPanicSafeAt(tc.pc); got != tc.want {
			t.Errorf("goplusPanicSafeAt(%s) = %v, want %v", tc.what, got, tc.want)
		}
	}
}
