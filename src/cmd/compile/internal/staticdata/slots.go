// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"slices"
	"strings"
	"sync"

	"cmd/compile/internal/objw"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
)

// SlotRegistry accumulates deferred data-slot symbols during parallel
// compilation and emits their contents once, sorted by symbol name,
// after all functions are compiled. Its zero value is ready for use.
//
// Slots are recorded (slot sym → target sym) while functions are
// compiled in parallel but NOT emitted there: emitting inline appends
// each slot to the object in goroutine-scheduling order, making the
// compiled object non-reproducible (trendvidia/go-archive#8 for go.funcptr,
// trendvidia/go-archive#27 for go.typeptr/go.itabptr). Write emits the
// recorded slots sorted by name instead.
//
// Emission order affects only object layout, not semantics: the slots
// are DUPOK and are resolved by name by the linker and by the runtime's
// module-load canonicalisation.
type SlotRegistry struct {
	mu      sync.Mutex              // serialises Record from parallel backend workers
	pending map[*obj.LSym]*obj.LSym // slot symbol → target it points at
}

// Record notes that slot's sole word holds target's address. The first
// recording wins; a slot's name determines its target, so
// first-writer-wins cannot diverge.
func (r *SlotRegistry) Record(slot, target *obj.LSym) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = make(map[*obj.LSym]*obj.LSym)
	}
	if _, ok := r.pending[slot]; !ok {
		r.pending[slot] = target
	}
}

// Write emits every recorded slot: a single pointer-sized word
// initialised to its target's address, DUPOK|NOPTR. It lands as
// SNOPTRDATA: objw.SymPtr's write makes the symbol SDATA
// (obj.(*LSym).prepwrite) and obj.Globl maps a NOPTR data symbol to
// SNOPTRDATA. That is what keeps it in writable .noptrdata rather than
// being promoted to a RELRO section by the linker's
// makeRelroForSharedLib (which boosts read-only kinds carrying
// relocations); the runtime rewrites slots at module load during
// cross-module canonicalisation, so a read-only slot would fault
// there. Assigning the type explicitly after Global was measured to
// change nothing (byte-identical object, 2026-08-27, trendvidia/go-archive#67); what pins
// the writable placement of these slots is
// cmd/compile/internal/test.TestTypetabSlotsPresent on the object and
// cmd/cgo/internal/testplugin.TestTypetabLinkedTables on the linked
// ELF plugin. (The same SymPtr-then-Global pattern in
// WriteFuncSyms's -dynlink/-shared arm is pinned separately, by
// TestFuncsymWritableUnderDynlink — neither typetab test compiles
// with those flags or takes a function value, so neither covers it.)
// Must be called once, single-threaded, after all functions are
// compiled — from gc.dumpdata.
func (r *SlotRegistry) Write() {
	r.mu.Lock()
	defer r.mu.Unlock()
	slots := make([]*obj.LSym, 0, len(r.pending))
	for s := range r.pending {
		slots = append(slots, s)
	}
	slices.SortFunc(slots, func(a, b *obj.LSym) int {
		return strings.Compare(a.Name, b.Name)
	})
	for _, s := range slots {
		objw.SymPtr(s, 0, r.pending[s], 0)
		objw.Global(s, int32(types.PtrSize), obj.DUPOK|obj.NOPTR)
	}
}
