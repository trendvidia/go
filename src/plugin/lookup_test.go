// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ((linux || freebsd || darwin) && cgo) || windows

// End-to-end pins for the glue liveorigin_test.go leaves uncovered:
// that lookup actually routes through liveOrigin, reads syms and
// pluginpath off the entry it returns rather than off its argument,
// and hands back lookupClosedErr's error when it resolves nothing
// (trendvidia/go#64). TestLiveOrigin and TestLookupClosedErr each pin
// one half; nothing until now pinned that lookup wires them together.
//
// Constrained to the platforms that have a real lookup: the build tag
// is the complement of plugin_stubs.go's, whose lookup returns "not
// implemented" without reading p at all.
//
// Nothing here loads a plugin. lookup only reads fields off the cache
// entry it resolves, so the fabricated entries liveorigin_test.go's
// helpers build are enough — which is the point, since a real load
// cannot produce a copy whose fields disagree with the cache's.
package plugin

import (
	"strings"
	"testing"
)

// TestLookupReadsTheCachedEntry drives a copy whose own pluginpath
// and syms both differ from the cached original's through lookup. If
// either were read off the argument the assertions below would see
// the copy's values.
func TestLookupReadsTheCachedEntry(t *testing.T) {
	p := live("plugin1")
	stale := *p
	stale.pluginpath = "stale/path"
	stale.syms = map[string]any{"F": Symbol(&fakeSym{"stale/path"})}

	withPlugins(t, map[string]*Plugin{"a": p}, func() {
		sym, err := lookup(&stale, "F")
		if err != nil {
			t.Fatalf("lookup(copy, F) = %v, want the cached entry's symbol", err)
		}
		if got := sym.(*fakeSym).path; got != "plugin1" {
			t.Errorf("lookup(copy, F) came from the %q syms map, want the cached entry's", got)
		}
		_, err = lookup(&stale, "NoSuchSymbol")
		if err == nil {
			t.Fatal("lookup(copy, NoSuchSymbol) = nil error, want a not-found error")
		}
		if !strings.Contains(err.Error(), "not found in plugin plugin1") {
			t.Errorf("lookup(copy, NoSuchSymbol) = %q, want the CACHED entry's path named", err)
		}
	})
}

// TestLookupOnValueNamingNoLivePlugin pins which values get the
// closed error out of lookup, and — the load-bearing half — which of
// them may have their pluginpath formatted into it. Naming the path
// means lookup dereferenced it, which is safe only once Close has
// cloned it into host memory.
func TestLookupOnValueNamingNoLivePlugin(t *testing.T) {
	cached := live("plugin1")

	// A copy taken before Close: its pluginpath is the pre-clone
	// header the image no longer maps, and it matches no cache entry.
	beforeClose := *live("plugin2")
	// The original after a completed Close: evicted from the cache,
	// err set, path already cloned into host memory.
	closedOriginal := &Plugin{pluginpath: "plugin2", err: closedErr}

	for _, tt := range []struct {
		name     string
		arg      *Plugin
		wantPath bool
	}{
		{"zero value", &Plugin{}, false},
		{"copy taken before Close", &beforeClose, false},
		{"failed load", &Plugin{pluginpath: "plugin2", err: "no Go plugin module data"}, false},
		{"closed original", closedOriginal, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withPlugins(t, map[string]*Plugin{"a": cached}, func() {
				_, err := lookup(tt.arg, "F")
				if err == nil {
					t.Fatal(`lookup() = nil error, want "plugin is closed"`)
				}
				if !strings.Contains(err.Error(), "plugin is closed") {
					t.Fatalf(`lookup() = %q, want a "plugin is closed" error`, err)
				}
				if named := strings.Contains(err.Error(), "plugin2"); named != tt.wantPath {
					t.Errorf("lookup() = %q, names the path = %v, want %v", err, named, tt.wantPath)
				}
			})
		})
	}
}
