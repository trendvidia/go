// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
)

// GlobalPtrLsym returns the linker symbol for the go.globalptr.<orig.Name>
// slot — a pointer-sized writable data location whose static initializer
// is the address of orig. Under -shared/-dynlink, the compiler routes
// global-variable references through this slot so the runtime can
// rewrite the slot at module load to point at the canonical instance
// (first-loader-wins) of orig across all loaded modules.
//
// DUPOK lets the linker dedup the slot across packages that reference
// the same global. NOPTR means the GC does not scan the slot itself —
// the original global's storage remains the GC root via its module's
// gcdata bitmap; the slot is only an indirection pointer.
//
// Callers in static-initializer contexts (where the value must be
// computable at link time) must NOT use this; emit a direct relocation
// to orig instead. The indirection is only valid in expression
// contexts that execute at runtime.
func GlobalPtrLsym(orig *obj.LSym) *obj.LSym {
	return slotLsym("go.globalptr.", orig)
}

// FuncPtrLsym returns the linker symbol for the go.funcptr.<orig.Name>
// slot — same shape as GlobalPtrLsym but for code-pointer indirection
// (closure code pointers, function values, method-value wrappers).
//
// Two distinct namespaces (go.globalptr vs go.funcptr) so the runtime
// and linker can separately enumerate data slots and code slots — Close
// converts only the funcptr table to panic-on-call stubs at unload,
// while the globalptr table follows a different invalidation path.
func FuncPtrLsym(orig *obj.LSym) *obj.LSym {
	return slotLsym("go.funcptr.", orig)
}

func slotLsym(prefix string, orig *obj.LSym) *obj.LSym {
	name := prefix + orig.Name
	s := base.Ctxt.Lookup(name)
	slotMu.Lock()
	if s.Size == 0 {
		objw.SymPtr(s, 0, orig, 0)
		objw.Global(s, int32(types.PtrSize), obj.DUPOK|obj.NOPTR)
		// Force SNOPTRDATA so the slot lives in writable .noptrdata
		// rather than being promoted to a RELRO read-only section by
		// makeRelroForSharedLib (which boosts ReadOnly symbol kinds
		// with relocations to *RELRO variants). The runtime needs to
		// write to the slot during canonicalisation at module load.
		s.Type = objabi.SNOPTRDATA
	}
	slotMu.Unlock()
	return s
}

// slotMu serialises slot creation. Without it, two goroutines
// compiling packages in parallel can both observe Size == 0 and both
// call objw.Global, producing a redeclaration error from the linker.
var slotMu sync.Mutex
