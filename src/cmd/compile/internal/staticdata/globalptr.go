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
	name := "go.globalptr." + orig.Name
	s := base.Ctxt.Lookup(name)
	globalPtrMu.Lock()
	if s.Size == 0 {
		objw.SymPtr(s, 0, orig, 0)
		objw.Global(s, int32(types.PtrSize), obj.DUPOK|obj.NOPTR)
	}
	globalPtrMu.Unlock()
	return s
}

// globalPtrMu serialises slot creation. Without it, two goroutines
// compiling packages in parallel can both observe Size == 0 and both
// call objw.Global, producing a redeclaration error from the linker.
var globalPtrMu sync.Mutex
