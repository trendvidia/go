// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"reflect"
	"runtime"

	"testplugin/funcident/shared"
	"testplugin/funcident/wrap"
)

// SharedVarPtr is the code pointer reached through the host-linked
// package's own func-valued var (row 1: the variable is host-shared).
func SharedVarPtr() uintptr { return reflect.ValueOf(shared.FuncVar).Pointer() }

// MaterializedPtr is the code pointer of a func value this module
// materializes at runtime, which goes through a go.funcptr slot.
func MaterializedPtr() uintptr { return reflect.ValueOf(shared.Fn).Pointer() }

// WrapPtr is the code pointer of the statically initialized word in a
// plugin-only package (trendvidia/go-archive#106's literal shape).
func WrapPtr() uintptr { return reflect.ValueOf(wrap.FuncVar).Pointer() }

// FnName asks the runtime to name the materialized value. Before the
// fix this answered ".rel" — the linker's thunk table.
func FnName() string {
	f := runtime.FuncForPC(reflect.ValueOf(shared.Fn).Pointer())
	if f == nil {
		return "<FuncForPC returned nil>"
	}
	return f.Name()
}

// Call proves the value is callable whichever way it resolves; it was
// callable even when identity was wrong, via the plugin's thunk.
func Call() int { return wrap.FuncVar() + shared.FuncVar() }

func main() {}
