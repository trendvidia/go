// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command canonicaldata checks that type descriptors, itabs, and data
// addresses the plugin embedded in STATIC data (trendvidia/go#25) were
// canonicalized onto this host's copies at plugin load. One edge per
// invocation, selected by the argument, so each has its own named test:
//
//	dict        rtype and itab words inside generic dictionaries
//	staticiface statically-initialized interface globals
//	assertdesc  type-assert descriptors baking the local interface type
//	dataaddr    addresses of cross-module globals embedded in data
//	staticfunc  a static func value of a host-shared function
package main

import (
	"fmt"
	"log"
	"os"
	"plugin"
	"runtime"

	"testplugin/canonicaldata/shared"
)

// pluginExt mirrors plugin_test.go's pluginSuffix; this fixture runs
// as a standalone executable, so it computes the suffix itself.
var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func lookup[F any](p *plugin.Plugin, name string) F {
	s, err := p.Lookup(name)
	if err != nil {
		log.Fatalf("Lookup(%q) failed: %v", name, err)
	}
	f, ok := s.(F)
	if !ok {
		log.Fatalf("Lookup(%q): unexpected symbol type %T", name, s)
	}
	return f
}

func checkDict(p *plugin.Plugin) {
	// The eface built inside the plugin's generic instantiation must
	// carry this host's shared.Concrete descriptor, or the assertion
	// (compiled here, against the canonical copy) cannot match.
	a := lookup[func() any](p, "MakeAny")()
	c, ok := a.(shared.Concrete)
	if !ok || c.V != 7 {
		log.Fatalf("MakeAny() = %v (%T); asserting the dictionary-built eface to shared.Concrete gave (%v, %v), want (7, true)", a, a, c.V, ok)
	}
	i := lookup[func() shared.I](p, "MakeI")()
	c, ok = i.(shared.Concrete)
	if !ok || c.V != 8 {
		log.Fatalf("MakeI(): asserting the dictionary-built iface to shared.Concrete gave (%v, %v), want (8, true)", c.V, ok)
	}
}

func checkStaticIface(p *plugin.Plugin) {
	e := lookup[func() error](p, "GetStaticErr")()
	me, ok := e.(shared.MyErr)
	if !ok || me.Code != 3 {
		log.Fatalf("GetStaticErr() = %v (%T); asserting to shared.MyErr gave (%v, %v), want (3, true)", e, e, me.Code, ok)
	}
	a := lookup[func() any](p, "GetStaticAny")()
	c, ok := a.(shared.Concrete)
	if !ok || c.V != 9 {
		log.Fatalf("GetStaticAny() = %v (%T); asserting to shared.Concrete gave (%v, %v), want (9, true)", a, a, c.V, ok)
	}
}

func checkAssertDesc(p *plugin.Plugin) {
	// Assert the same eface through the plugin's descriptor and through
	// this host's. Both interfaces wrap the identical data word, so the
	// results compare equal exactly when the two asserts produced the
	// same itab — i.e. when the plugin's descriptor was canonicalized
	// instead of minting a second itab from its local shared.I.
	c := shared.Concrete{V: 4}
	var v any = c
	got, ok := lookup[func(any) (shared.I, bool)](p, "AsI")(v)
	if !ok {
		log.Fatalf("plugin AsI(shared.Concrete{4}) reported not-ok")
	}
	want, ok := v.(shared.I)
	if !ok {
		log.Fatalf("host-side v.(shared.I) reported not-ok")
	}
	if got != want {
		log.Fatalf("plugin AsI and host assert of the same eface disagree (%#v != %#v): the plugin's assert descriptor minted a second itab", got, want)
	}
}

func checkDataAddr(p *plugin.Plugin) {
	if got, want := lookup[func() *int](p, "PtrGlobal")(), &shared.Global; got != want {
		log.Fatalf("plugin PtrGlobal() = %p, host &shared.Global = %p; want one instance", got, want)
	}
	if got, want := lookup[func() *int](p, "PtrArr2")(), &shared.Arr[2]; got != want {
		log.Fatalf("plugin PtrArr2() = %p, host &shared.Arr[2] = %p; want one instance", got, want)
	}
	shared.Global = 42
	if got := lookup[func() int](p, "ReadGlobal")(); got != 42 {
		log.Fatalf("plugin ReadGlobal() = %d after host wrote 42; the modules see different variables", got)
	}
}

func checkStaticFunc(p *plugin.Plugin) {
	// The plugin's shared.Fn·f funcval holds an address the linker
	// resolved to the PLT stub that redirects into this host. Calling
	// through it must land in shared.Fn; a scan that mistook the
	// funcval's TEXT target for a data address would have overwritten
	// the word with the stub's own instruction bytes, and this call
	// would jump to garbage instead.
	//
	// The host calls shared.Fn itself so the symbol is exported from
	// this image and the plugin's copy takes the host-shared shape.
	if got := shared.Fn(); got != 11 {
		log.Fatalf("host shared.Fn() = %d, want 11", got)
	}
	if got := lookup[func() int](p, "CallFn")(); got != 11 {
		log.Fatalf("plugin CallFn() = %d, want 11: the static func value did not reach shared.Fn", got)
	}
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: canonicaldata dict|staticiface|assertdesc|dataaddr|staticfunc")
	}
	p, err := plugin.Open("canonicaldata-plugin" + pluginExt)
	if err != nil {
		log.Fatalf("plugin.Open failed: %v", err)
	}
	switch os.Args[1] {
	case "dict":
		checkDict(p)
	case "staticiface":
		checkStaticIface(p)
	case "assertdesc":
		checkAssertDesc(p)
	case "dataaddr":
		checkDataAddr(p)
	case "staticfunc":
		checkStaticFunc(p)
	default:
		log.Fatalf("unknown edge %q", os.Args[1])
	}
	fmt.Println("PASS")
}
