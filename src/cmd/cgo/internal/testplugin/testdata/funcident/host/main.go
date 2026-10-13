// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests cross-module FUNC VALUE identity (trendvidia/go-archive#106).
//
// A func value is a pointer to a one-word funcval object; the compiler
// emits one `pkg.Fn·f` per module that uses Fn as a value. When Fn's
// body is the host's, a plugin's own copy holds the address of the
// plugin's IAT thunk, so calls work while every identity check sees a
// different pointer — and runtime.FuncForPC named such a value ".rel",
// the linker's thunk table, where POSIX names the function.
//
// Nothing canonicalised those copies because an exe-built host
// published no funcvals at all: WriteFuncSyms leaves `·f` SRODATA
// outside -dynlink, and userDataLink's export walk covered only the
// writable data kinds. Peer plugins agreed with each other (both are
// -dynlink, both register) but never with the host.
//
// Four shapes, all of which must agree with the host:
//
//   - shared.FuncVar: var AND func in the host-linked package. The
//     variable itself is host-shared, so no module reads a local copy.
//     Never broken; pinned so a fix cannot regress it.
//   - shared.Fn materialized through reflect: goes through a
//     go.funcptr slot. Was broken.
//   - wrap.FuncVar: statically initialized word in a plugin-only
//     package pointing at a host-shared func. trendvidia/go-archive#106's literal shape.
//     Was broken.
//   - plugA vs plugB: peer agreement, which worked before and must
//     keep working.
package main

import (
	"fmt"
	"os"
	"plugin"
	"reflect"
	"runtime"

	"testplugin/funcident/shared"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

const wantName = "testplugin/funcident/shared.Fn"

func main() {
	hostPtr := reflect.ValueOf(shared.Fn).Pointer()
	if got := funcName(hostPtr); got != wantName {
		fail("host: FuncForPC(shared.Fn) = %q, want %q", got, wantName)
	}

	var first map[string]uintptr
	for _, name := range []string{"funcident-plugA", "funcident-plugB"} {
		p, err := plugin.Open(name + pluginExt)
		if err != nil {
			fail("Open(%s): %v", name, err)
		}
		vals := map[string]uintptr{}
		for _, sym := range []string{"SharedVarPtr", "MaterializedPtr", "WrapPtr"} {
			f, err := p.Lookup(sym)
			if err != nil {
				fail("%s.Lookup(%s): %v", name, sym, err)
			}
			vals[sym] = f.(func() uintptr)()
		}

		// Every shape must name the SAME funcval the host uses.
		for _, sym := range []string{"SharedVarPtr", "MaterializedPtr", "WrapPtr"} {
			if vals[sym] != hostPtr {
				fail("%s: %s() = %#x, host has %#x — the plugin kept its own funcval, so cross-module "+
					"func identity is broken (calls still work through the plugin's thunk)", name, sym, vals[sym], hostPtr)
			}
		}

		// The sharpest symptom: a plugin-local funcval resolves to the
		// linker's thunk table, so the runtime names it ".rel".
		fn, err := p.Lookup("FnName")
		if err != nil {
			fail("%s.Lookup(FnName): %v", name, err)
		}
		if got := fn.(func() string)(); got != wantName {
			fail("%s: FuncForPC(shared.Fn).Name() = %q, want %q", name, got, wantName)
		}

		call, err := p.Lookup("Call")
		if err != nil {
			fail("%s.Lookup(Call): %v", name, err)
		}
		if got := call.(func() int)(); got != 14 {
			fail("%s: Call() = %d, want 14", name, got)
		}

		if first == nil {
			first = vals
		} else {
			for _, sym := range []string{"SharedVarPtr", "MaterializedPtr", "WrapPtr"} {
				if vals[sym] != first[sym] {
					fail("plugA and plugB disagree on %s: %#x vs %#x", sym, first[sym], vals[sym])
				}
			}
		}
	}

	fmt.Println("funcident ok")
}

func funcName(pc uintptr) string {
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "<FuncForPC returned nil>"
	}
	return f.Name()
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "funcident: "+format+"\n", args...)
	os.Exit(1)
}
