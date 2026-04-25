// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Exercises v2's funcptr panic-on-call stub conversion: a func value
// returned by Plugin.Lookup must, after Plugin.Close, panic with a
// recognisable message rather than jumping into now-unmapped (or
// soon-to-be-unmapped) plugin text.
//
// Held f() before Close: must succeed. Held f() after Close: must
// panic, and the recovered value must mention Plugin.Close.
package main

import (
	"fmt"
	"os"
	"plugin"
	"strings"
)

func main() {
	p, err := plugin.Open("plugin1.so")
	if err != nil {
		fail("Open(plugin1.so): %v", err)
	}
	sym, err := p.Lookup("F")
	if err != nil {
		fail("Lookup(F): %v", err)
	}
	f, ok := sym.(func() int)
	if !ok {
		fail("Lookup(F) is %T; want func() int", sym)
	}

	// Pre-Close call: ordinary success path. plugin1.F returns 3.
	if got := f(); got != 3 {
		fail("pre-Close f() = %d; want 3", got)
	}

	if err := p.Close(); err != nil {
		fail("Close(): %v", err)
	}

	// Post-Close call: must panic. We don't care about the return
	// value (control flow never reaches it); we recover and inspect
	// the panic value's message.
	got := postCloseCall(f)
	if got == "" {
		fail("post-Close f() did not panic; want recognisable Plugin.Close message")
	}
	if !strings.Contains(got, "Plugin.Close") {
		fail("post-Close panic = %q; want substring %q", got, "Plugin.Close")
	}

	fmt.Println("panicstub ok")
}

// postCloseCall invokes f and returns the recovered panic value's
// string form, or "" if f returned without panicking.
func postCloseCall(f func() int) (recovered string) {
	defer func() {
		switch r := recover().(type) {
		case nil:
			recovered = ""
		case string:
			recovered = r
		case error:
			recovered = r.Error()
		default:
			recovered = fmt.Sprintf("%v", r)
		}
	}()
	_ = f()
	return ""
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "panicstub: "+format+"\n", args...)
	os.Exit(1)
}
