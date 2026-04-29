// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"strings"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
)

// FuncPtrLsym returns the linker symbol for the go.funcptr.<orig.Name>
// slot — a pointer-sized writable data location whose static initializer
// is the address of orig. Under -shared/-dynlink, the compiler routes
// function-value references (closure code pointers, function values,
// method-value wrappers) through this slot so the runtime can rewrite
// the slot at module load to point at the canonical instance
// (first-loader-wins) of orig across all loaded modules. Pointer
// identity matters here — `f1 == f2` for func values stored in
// interfaces requires the same address everywhere, which POSIX dynamic
// linking doesn't natively guarantee for Go's higher-level value
// semantics.
//
// DUPOK lets the linker dedup the slot across packages that reference
// the same function. NOPTR means the GC does not scan the slot itself —
// the original function's storage remains the GC root via its module's
// gcdata bitmap; the slot is only an indirection pointer.
//
// (DATA accesses do not use a slot indirection: Bug D found that the
// 3-deref globalptr-style pattern collides with the IAT semantics on
// Windows internal-link plugins, and on POSIX the OS dynamic linker's
// GOT resolution already provides cross-module data sharing. The
// compiler emits a direct extern reference for PEXTERN; the linker
// routes through `__imp_X` on Windows or the GOT on POSIX.)
func FuncPtrLsym(orig *obj.LSym) *obj.LSym {
	return slotLsym("go.funcptr.", orig)
}

func slotLsym(prefix string, orig *obj.LSym) *obj.LSym {
	if isCompilerInternal(orig.Name) {
		// Compiler-internal symbols (statictmps, gcdata, inittask
		// etc.) are scoped to their emitting module and never
		// referenced cross-module — slot indirection adds binary
		// bloat with no canonicalisation benefit. Caller falls back
		// to the direct-addressing path. Also keeps these symbols
		// out of the post-link symbol table, satisfying tests like
		// dwarf_test.go::TestStaticTmp.
		return nil
	}
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

// isCompilerInternal reports whether name identifies a per-module
// symbol that shouldn't get a cross-module indirection slot.
//
// Two categories:
//
//  1. Compiler-generated names containing a literal `..` —
//     statictmps (runtime..stmp_NNN), gcdata (<pkg>..gcdata),
//     inittask (<pkg>..inittask), etc. The double-dot is Go's
//     convention; user-visible package members can never contain it.
//
//  2. Symbols in the `runtime` package or `internal/` packages —
//     these include per-module-instance state (runtime.firstmoduledata,
//     runtime.lastmoduledatap) that MUST stay local to its emitting
//     module. Canonicalising plugin's runtime.firstmoduledata to the
//     host's value breaks plugin registration: addmoduledata would
//     receive the host's already-registered struct instead of the
//     plugin's new one. Other runtime globals (e.g. runtime.GOOS) are
//     duplicated harmlessly across modules — each has the same value
//     at link time. Direct addressing within each module is correct.
//
// Other packages — including the standard library and user code — go
// through indirection so that plugin and host see the same canonical
// instance of, e.g., os.Args, fmt's interface tables, or a user's
// `var Counter int` exported across module boundaries.
func isCompilerInternal(name string) bool {
	if strings.Contains(name, "..") {
		return true
	}
	if strings.HasPrefix(name, "runtime.") {
		return true
	}
	if strings.HasPrefix(name, "internal/") {
		return true
	}
	return false
}

// slotMu serialises slot creation. Without it, two goroutines
// compiling packages in parallel can both observe Size == 0 and both
// call objw.Global, producing a redeclaration error from the linker.
var slotMu sync.Mutex
