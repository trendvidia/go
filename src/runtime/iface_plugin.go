// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Removal of a closed plugin's itabs from the global itab table.
//
// Upstream never removes an itab, because upstream never unloads a
// module: addModuleItabs only ever grows the table. This fork does
// unload, and Plugin.Close unmaps the image and reserves its address
// range (trendvidia/go-archive#122, #123), so every itab the closed module
// contributed becomes a pointer into inaccessible memory.
//
// That is not a dangling reference the caller can be blamed for.
// itabTableType.find dereferences each non-nil entry it probes to
// compare Inter and Type, so a single stale entry makes an UNRELATED
// interface lookup fault -- measured as `fmt.Printf` on a time.Duration
// dying in find, in a host that had merely opened and closed plugins
// (trendvidia/go-archive#150).
//
// It lives here rather than in iface.go so upstream's file stays
// byte-identical and keeps merging cleanly (trendvidia/go-archive#140's rule).

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// itabsPurgeModule removes from the itab table every itab that lives in
// md's itab region, by rebuilding the table without them and publishing
// the replacement.
//
// The table is rebuilt rather than edited because it is open-addressed
// with quadratic probing: clearing an entry in place truncates the probe
// sequence of everything inserted after it, which would turn a stale
// entry into silently missing lookups.
//
// itabLock must be held. The replacement is published with an atomic
// store, the same way itabAdd publishes a grown table, so a reader that
// has already loaded the old pointer keeps walking a consistent table.
//
// That reader is the residual window, and it is worth stating precisely.
// A goroutine preempted inside find between reading a non-nil entry and
// dereferencing it resumes holding a pointer this purge has removed, and
// faults if the image is gone by then. Reaching it requires the entry it
// had just read to belong to the module being closed -- that is, the
// program was resolving an interface for one of the closing plugin's own
// types while closing it, which Plugin.Close already documents as
// unsupported. The bug this fixes is the opposite case: a host touching
// nothing of the plugin's at all.
func itabsPurgeModule(md *moduledata) {
	if md.itabsize == 0 {
		return
	}
	lo := md.types + md.itaboffset
	hi := lo + md.itabsize

	t := itabTable
	// Count what survives before allocating, so the replacement is
	// sized from data and the throw below has something to check.
	var survivors uintptr
	for i := uintptr(0); i < t.size; i++ {
		m := *(**itab)(add(unsafe.Pointer(&t.entries), i*goarch.PtrSize))
		if m == nil || itabInRange(m, lo, hi) {
			continue
		}
		survivors++
	}
	if survivors == t.count {
		// The module contributed nothing that is still registered.
		return
	}

	// Same size as the current table: count only shrinks here, so the
	// load factor cannot get worse and no growth decision is needed.
	// The size formula matches itabAdd's; see the note on
	// itabTableType.
	t2 := (*itabTableType)(mallocgc((2+t.size)*goarch.PtrSize, nil, true))
	t2.size = t.size
	for i := uintptr(0); i < t.size; i++ {
		m := *(**itab)(add(unsafe.Pointer(&t.entries), i*goarch.PtrSize))
		if m == nil || itabInRange(m, lo, hi) {
			continue
		}
		t2.add(m)
	}
	if t2.count != survivors {
		throw("runtime: itab purge lost entries")
	}
	atomicstorep(unsafe.Pointer(&itabTable), unsafe.Pointer(t2))
}

// itabInRange reports whether m lies in [lo, hi).
func itabInRange(m *itab, lo, hi uintptr) bool {
	p := uintptr(unsafe.Pointer(m))
	return p >= lo && p < hi
}
