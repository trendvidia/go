// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"strings"

	"cmd/compile/internal/base"
	"cmd/internal/obj"
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
// The slots are taken on -shared as well as -dynlink, so every PIE
// build gets them: every default darwin build and linux -buildmode=pie,
// not only plugins. Most of those binaries never load a plugin, and a
// host does not need slots to agree with plugins on function identity:
// linux executable hosts have none and pass the identity suites through
// the funcval export path (trendvidia/go#106). Restricting the slots to
// -dynlink was measured to pass every plugin suite on darwin and to save
// 1.09% of file size in belluikit/gallery (4,092 slots). It was declined
// to keep what PIE hosts do at load unchanged (trendvidia/go#161).
// TestPIEBuildsTakeFuncPtrSlots fails if they stop getting slots.
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
	if obj.TrimInlineHash(orig.Name) != orig.Name {
		// A closure created by inlining carries a #hash# suffix that
		// keeps it apart from its siblings only inside the compiler:
		// the object writer strips it, so every copy of the closure
		// reaches the linker and the runtime under one name. Go 1.27
		// specialises those copies on their constant captures, so
		// they need not share a body. A DUPOK slot under the trimmed
		// name would make the linker keep one slot for all of them,
		// and every copy would call the first body (trendvidia/go#158).
		// The name does not identify the function across modules
		// either, so canonicalising by it would be just as wrong.
		// Address it directly instead.
		return nil
	}
	name := prefix + orig.Name
	s := base.Ctxt.Lookup(name)
	// Record the slot for deferred, order-stable emission. We do NOT
	// emit the slot data here: slotLsym is called from ssagen while
	// functions are compiled in parallel, so emitting inline appends
	// the slot to the object in goroutine-scheduling order and makes
	// the compiled object non-reproducible (trendvidia/go#8).
	// WriteFuncPtrSlots emits the recorded slots sorted by name at the
	// end of compilation instead. The caller only needs the symbol to
	// exist (as a relocation target); its data is filled in before the
	// object is written — the same deferred pattern WriteFuncSyms uses.
	funcPtrSlots.Record(s, orig)
	return s
}

// funcPtrSlots records every go.funcptr slot referenced during
// compilation; drained, sorted, by WriteFuncPtrSlots.
var funcPtrSlots SlotRegistry

// WriteFuncPtrSlots emits the deferred go.funcptr slot data recorded by
// slotLsym, in a deterministic order (sorted by symbol name). Must be
// called once, single-threaded, after all functions are compiled — from
// gc.dumpdata, alongside WriteFuncSyms. See SlotRegistry.Write for why
// emission is deferred and what the emitted slot looks like.
func WriteFuncPtrSlots() {
	funcPtrSlots.Write()
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
