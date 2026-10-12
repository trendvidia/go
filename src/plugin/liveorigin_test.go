// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// White-box pins for the *Plugin resolution lookup does before it
// touches any symbol table (trendvidia/go-archive#64).
//
// The end-to-end proof lives in cmd/cgo/internal/testplugin's
// lookupcopy fixture, but that test is globalSkip'd under -short —
// which is what cmd/dist passes to every package — so on an ordinary
// all.bash it guards nothing. The tests here run in-process on every
// platform, including the ones plugin_stubs.go serves, because
// liveOrigin, lookupClosedErr and the plugins cache all live in
// plugin.go and carry no build constraint; lookup_test.go then wires
// the two units together through lookup itself, on the platforms
// that have a real one.
//
// They also reach the states the fixture cannot construct: a cache
// holding entries for OTHER plugins (the fixture's cache never holds
// more than one), and a cached entry that is closed-or-failed while
// its loaded channel is still non-nil (the fixture only ever sees an
// entry that Close EVICTED). That second state is the only one in
// which liveOrigin's inner err check does any work: it is what open's
// "could not find symbol" path leaves behind, and what Close leaves
// behind when unloadPlugin fails.
//
// Two more states no fixture here can construct, pinned because they
// are assumptions rather than observations: an entry open has cached
// but not finished publishing (an OPEN loaded channel and no syms),
// and a nil *Plugin.
package plugin

import (
	"strings"
	"testing"
)

// withPlugins installs cache as the package-level plugins map for the
// duration of fn, restoring the real one afterwards. The plugin
// package under test never loads anything, so the real map is nil,
// but restoring keeps these tests independent of run order.
//
// It deliberately does NOT hold pluginsMu across fn. sync.Mutex is
// not reentrant, so a test that drives lookup end to end — which
// takes the mutex itself, and which lookup_test.go does — would
// self-deadlock and hang the package instead of failing. Tests that
// call liveOrigin or lookupClosedErr directly must satisfy the
// "caller holds pluginsMu" precondition those two document; locked
// below is how they do it.
func withPlugins(t *testing.T, cache map[string]*Plugin, fn func()) {
	t.Helper()
	swap := func(m map[string]*Plugin) map[string]*Plugin {
		pluginsMu.Lock()
		defer pluginsMu.Unlock()
		prev := plugins
		plugins = m
		return prev
	}
	saved := swap(cache)
	defer swap(saved)
	fn()
}

// locked runs fn with pluginsMu held, the precondition liveOrigin
// and lookupClosedErr document.
func locked(fn func()) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	fn()
}

// fakeSym stands in for a plugin symbol. lookup only ever compares
// the map value against nil and hands it back, so any non-nil value
// carrying a recognisable identity will do; path records which entry
// the symbol was built for, which is how lookup_test.go tells the
// cached entry's syms map from a stale copy's.
type fakeSym struct{ path string }

// live builds the shape open leaves in the cache for a successful
// load: a closed loaded channel, no err, a syms map.
//
// The Symbol is non-nil on purpose. lookup's hit test is
// "if s := origin.syms[symName]; s != nil", so a nil interface here
// would make a symbol that IS in the map read as missing, hollowing
// out any assertion routed through lookup.
func live(path string) *Plugin {
	ch := make(chan struct{})
	close(ch)
	return &Plugin{
		pluginpath: path,
		loaded:     ch,
		syms:       map[string]any{"F": Symbol(&fakeSym{path})},
	}
}

func TestLiveOrigin(t *testing.T) {
	// Neighbours the walk must not confuse anything with: another
	// live plugin, a load that failed before a channel existed, and
	// a load that failed after one did.
	other := live("other/plugin")
	failedEarly := &Plugin{pluginpath: "bad/early", err: "no Go plugin module data"}
	failedLate := live("bad/late")
	failedLate.err = "could not find symbol X: dlsym failed"

	p := live("plugin1")
	copyOfP := *p

	closed := live("plugin2")
	closedCopy := *closed
	closed.syms = nil
	closed.err = closedErr

	// Re-Open of plugin1's path after a Close: a distinct entry with
	// its own channel, cached under the same key.
	reopened := live("plugin1")

	// The one live shape the fixture cannot reach: open caches the
	// entry, drops pluginsMu, and only THEN runs doInit and fills
	// syms, so between those points the cache holds an entry whose
	// loaded channel is still open and whose syms is nil.
	inFlight := &Plugin{pluginpath: "plugin3", loaded: make(chan struct{})}

	tests := []struct {
		name  string
		cache map[string]*Plugin
		arg   *Plugin
		want  *Plugin
	}{
		{"zero value", map[string]*Plugin{"a": other}, &Plugin{}, nil},
		{"empty cache", nil, p, nil},
		{"original resolves to itself", map[string]*Plugin{"a": p}, p, p},
		{"copy resolves to the original", map[string]*Plugin{"a": p}, &copyOfP, p},
		{
			// The fixture's cache never holds more than one entry, so
			// it cannot show that the walk picks the right one.
			"copy resolves past neighbours",
			map[string]*Plugin{"a": other, "b": failedEarly, "c": failedLate, "d": p},
			&copyOfP, p,
		},
		{
			// Close evicted the original: nothing matches.
			"evicted original",
			map[string]*Plugin{"a": other},
			p, nil,
		},
		{"copy of an evicted original", map[string]*Plugin{"a": other}, &copyOfP, nil},
		{
			// A re-Open of the same path gets a fresh channel, so the
			// stale copy must not latch onto the new registration.
			"stale copy does not match a re-Open",
			map[string]*Plugin{"a": reopened},
			&copyOfP, nil,
		},
		{
			// unloadPlugin failed, so Close left the entry cached with
			// err set and loaded still non-nil. Only the inner err
			// check rejects this; the walk finds it.
			"closed entry still cached",
			map[string]*Plugin{"a": closed},
			closed, nil,
		},
		{"copy of a still-cached closed entry", map[string]*Plugin{"a": closed}, &closedCopy, nil},
		{
			// open's could-not-find-symbol path: cached, err set,
			// channel closed, syms nil. Reached through a copy, since
			// open never hands this entry out.
			"failed load that got as far as a channel",
			map[string]*Plugin{"a": failedLate},
			failedLate, nil,
		},
		{"failed load with no channel", map[string]*Plugin{"a": failedEarly}, failedEarly, nil},
		{
			// liveOrigin resolves an entry open has cached but not
			// finished publishing: it tests err, not readiness. That
			// is unreachable today — the *Plugin does not escape open
			// until close(p.loaded), so no caller can hold a value
			// sharing that channel — and this case pins the
			// reachability assumption rather than the shape. If it
			// ever breaks, Lookup starts reporting "symbol F not
			// found" for symbols that are merely not filled in yet,
			// and here is where to notice.
			"entry still being published",
			map[string]*Plugin{"a": inFlight},
			inFlight, inFlight,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withPlugins(t, tt.cache, func() {
				var got *Plugin
				locked(func() { got = liveOrigin(tt.arg) })
				if got != tt.want {
					t.Errorf("liveOrigin() = %p, want %p", got, tt.want)
				}
			})
		})
	}
}

// TestLiveOriginNeverReadsArgFields pins the property the whole fix
// rests on: whatever liveOrigin is handed, the syms map and
// pluginpath it reports come off the CACHED entry. A copy taken
// before Close carries stale values for both; if either were read off
// the argument, the values below would surface.
func TestLiveOriginNeverReadsArgFields(t *testing.T) {
	p := live("real/path")
	stale := *p
	stale.pluginpath = "stale/path"
	stale.syms = map[string]any{"Ghost": Symbol(nil)}

	withPlugins(t, map[string]*Plugin{"a": p}, func() {
		var origin *Plugin
		locked(func() { origin = liveOrigin(&stale) })
		if origin != p {
			t.Fatalf("liveOrigin(copy) = %p, want the cached entry %p", origin, p)
		}
		if origin.pluginpath != "real/path" {
			t.Errorf("origin.pluginpath = %q, want the cached entry's path", origin.pluginpath)
		}
		if _, ok := origin.syms["Ghost"]; ok {
			t.Error("origin.syms is the copy's stale map, want the cached entry's")
		}
	})
}

func TestLookupClosedErr(t *testing.T) {
	// A completed Close cloned pluginpath into host memory, so the
	// error may name it.
	closed := &Plugin{pluginpath: "some/plugin", err: closedErr}
	var got string
	locked(func() { got = lookupClosedErr(closed).Error() })
	if !strings.Contains(got, "some/plugin") {
		t.Errorf("lookupClosedErr(closed) = %q, want the path named", got)
	}

	// Everything else may hold a pluginpath into an unmapped image,
	// so the error must not name it — formatting it is the fault this
	// error exists to avoid. A copy taken before Close is the case
	// that matters; the zero value and a failed load take the same
	// branch.
	for _, tt := range []struct {
		name string
		p    *Plugin
	}{
		{"copy taken before Close", &Plugin{pluginpath: "dangling/path", loaded: make(chan struct{})}},
		{"zero value", &Plugin{}},
		{"failed load", &Plugin{pluginpath: "dangling/path", err: "no Go plugin module data"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			locked(func() { got = lookupClosedErr(tt.p).Error() })
			if strings.Contains(got, "dangling/path") {
				t.Errorf("lookupClosedErr() = %q, want no path named", got)
			}
			if !strings.Contains(got, "plugin is closed") {
				t.Errorf("lookupClosedErr() = %q, want a \"plugin is closed\" error", got)
			}
		})
	}
}

// TestLiveOriginNilPanics pins the nil-receiver behaviour
// [Plugin.Lookup]'s doc comment states: Lookup dereferences its
// receiver, so a nil *Plugin panics instead of returning an error —
// unlike Plugin.Close, which closePlugin makes a documented no-op on
// nil. liveOrigin's p.loaded read is where the dereference happens.
// lookup takes pluginsMu with defer precisely so a panic here
// releases it instead of wedging every later Open/Lookup/Close, which
// is why locked (also deferred) is the right shape to raise it in.
func TestLiveOriginNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("liveOrigin(nil) did not panic; Plugin.Lookup's doc says a nil *Plugin does")
		}
	}()
	withPlugins(t, map[string]*Plugin{"a": live("plugin1")}, func() {
		locked(func() { liveOrigin(nil) })
	})
}
