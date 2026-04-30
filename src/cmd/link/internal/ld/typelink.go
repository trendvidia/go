// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"slices"
	"strings"
)

type typelinkSortKey struct {
	TypeStr string
	Type    loader.Sym
}

// typelink generates the typelink table which is used by reflect.typelinks().
// Types that should be added to the typelinks table are marked with the
// MakeTypelink attribute by the compiler.
func (ctxt *Link) typelink() {
	ldr := ctxt.loader
	var typelinks []typelinkSortKey
	var itabs []loader.Sym
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		if ldr.IsTypelink(s) {
			typelinks = append(typelinks, typelinkSortKey{decodetypeStr(ldr, ctxt.Arch, s), s})
		} else if ldr.IsItab(s) {
			itabs = append(itabs, s)
		}
	}
	slices.SortFunc(typelinks, func(a, b typelinkSortKey) int {
		return strings.Compare(a.TypeStr, b.TypeStr)
	})

	tl := ldr.CreateSymForUpdate("runtime.typelink", 0)
	tl.SetType(sym.STYPELINK)
	ldr.SetAttrLocal(tl.Sym(), true)
	tl.SetSize(int64(4 * len(typelinks)))
	tl.Grow(tl.Size())
	relocs := tl.AddRelocs(len(typelinks))
	for i, s := range typelinks {
		r := relocs.At(i)
		r.SetSym(s.Type)
		r.SetOff(int32(i * 4))
		r.SetSiz(4)
		r.SetType(objabi.R_ADDROFF)
	}

	ptrsize := ctxt.Arch.PtrSize
	il := ldr.CreateSymForUpdate("runtime.itablink", 0)
	il.SetType(sym.SITABLINK)
	ldr.SetAttrLocal(il.Sym(), true)
	il.SetSize(int64(ptrsize * len(itabs)))
	il.Grow(il.Size())
	relocs = il.AddRelocs(len(itabs))
	for i, s := range itabs {
		r := relocs.At(i)
		r.SetSym(s)
		r.SetOff(int32(i * ptrsize))
		r.SetSiz(uint8(ptrsize))
		r.SetType(objabi.R_ADDR)
	}

	// typeptrs / typeptroffs: per-module table of go.typeptr.<T> slot
	// addresses and the typeOff of the <T> they resolve to. The runtime
	// walks this at plugin load and rewrites each slot to the canonical
	// *_type pointer via md.typemap (populated by typelinksinit) — so
	// plugin code reading the slot sees the host's type descriptor
	// whenever the type is shared across modules. Produced only when
	// the compiler was invoked with -typetab (Windows plugin mode).
	var typeptrs []loader.Sym
	typeptrTypes := make(map[loader.Sym]loader.Sym) // slot -> underlying type:<T>
	seenTypeptr := make(map[string]bool)
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		name := ldr.SymName(s)
		if !strings.HasPrefix(name, "go.typeptr.") {
			continue
		}
		// DUPOK symbols can produce multiple loader.Sym IDs under the
		// same name. Keep only the canonical one with the expected
		// pointer-sized payload and its R_ADDR relocation to the type.
		if seenTypeptr[name] {
			continue
		}
		if ldr.SymSize(s) != int64(ptrsize) {
			continue
		}
		srelocs := ldr.Relocs(s)
		if srelocs.Count() < 1 {
			continue
		}
		seenTypeptr[name] = true
		typeptrs = append(typeptrs, s)
		typeptrTypes[s] = srelocs.At(0).Sym()
	}

	tp := ldr.CreateSymForUpdate("runtime.typeptrs", 0)
	tp.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(tp.Sym(), true)
	tp.SetSize(int64(ptrsize * len(typeptrs)))
	tp.Grow(tp.Size())
	if len(typeptrs) > 0 {
		relocs = tp.AddRelocs(len(typeptrs))
		for i, s := range typeptrs {
			r := relocs.At(i)
			r.SetSym(s)
			r.SetOff(int32(i * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
	}

	tpo := ldr.CreateSymForUpdate("runtime.typeptroffs", 0)
	tpo.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(tpo.Sym(), true)
	tpo.SetSize(int64(4 * len(typeptrs)))
	tpo.Grow(tpo.Size())
	if len(typeptrs) > 0 {
		relocs = tpo.AddRelocs(len(typeptrs))
		for i, s := range typeptrs {
			r := relocs.At(i)
			r.SetSym(typeptrTypes[s])
			r.SetOff(int32(i * 4))
			r.SetSiz(4)
			r.SetType(objabi.R_ADDROFF)
		}
	}

	// itabptrs: per-module table of go.itabptr.<X> slot addresses. Each
	// slot references a go.itab.<X> symbol. At plugin load the runtime
	// reads the itab's Inter and Type, canonicalizes each via md.typemap
	// and getitab, then rewrites the slot with the canonical itab — so
	// non-empty interface values built in the plugin carry the same itab
	// as host / other modules. Produced only when -typetab was passed.
	var itabptrs []loader.Sym
	seenItabptr := make(map[string]bool)
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		name := ldr.SymName(s)
		if !strings.HasPrefix(name, "go.itabptr.") {
			continue
		}
		// DUPOK symbols can produce multiple loader.Sym IDs under the
		// same name. Keep only the canonical one with the expected
		// pointer-sized payload and its R_ADDR relocation to the itab.
		if seenItabptr[name] {
			continue
		}
		if ldr.SymSize(s) != int64(ptrsize) {
			continue
		}
		srelocs := ldr.Relocs(s)
		if srelocs.Count() < 1 {
			continue
		}
		seenItabptr[name] = true
		itabptrs = append(itabptrs, s)
	}

	ip := ldr.CreateSymForUpdate("runtime.itabptrs", 0)
	ip.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(ip.Sym(), true)
	ip.SetSize(int64(ptrsize * len(itabptrs)))
	ip.Grow(ip.Size())
	if len(itabptrs) > 0 {
		relocs = ip.AddRelocs(len(itabptrs))
		for i, s := range itabptrs {
			r := relocs.At(i)
			r.SetSym(s)
			r.SetOff(int32(i * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
	}
}

// funcptrlink gathers all per-module go.funcptr.* indirection slots
// emitted by the compiler under -shared/-dynlink and builds three
// carrier symbols:
//
//   - runtime.funcptrlinks — pointer-sized slot addresses (R_ADDR
//     relocations to each slot).
//   - runtime.funcptrnameoffs — int32 offsets into the names blob,
//     one per slot.
//   - runtime.funcptrnames — packed null-terminated target names
//     (extracted from the slot symbol's name by stripping the
//     go.funcptr. prefix).
//
// moduledata exposes the three carriers as parallel slices so the
// runtime's canonicalisation pass can map each slot to its target's
// name and look up the canonical (first-loader-wins) instance across
// all loaded modules — needed so func values stored in interfaces
// have stable address identity across host and plugin.
//
// In -buildmode=exe the compiler emits no slot symbols, so all
// carriers remain zero-sized and runtime canonicalisation is a no-op.
//
// (Data-side indirection was previously emitted as go.globalptr.*
// slots; that path was removed in the Bug D fix because it collided
// with the IAT semantics the Go internal linker provides for cross-
// module data on Windows. Cross-module data sharing now goes through
// the linker's native mechanism on each platform — the IAT on Windows,
// the GOT on POSIX.)
func (ctxt *Link) funcptrlink() {
	ldr := ctxt.loader
	var funcptrs []loader.Sym
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		name := ldr.SymName(s)
		if strings.HasPrefix(name, "go.funcptr.") {
			funcptrs = append(funcptrs, s)
		}
	}
	slices.SortFunc(funcptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})

	ctxt.emitSlotCarriers("funcptr", funcptrs)
}

// isUserPackageName reports whether name belongs to a user (non-stdlib)
// Go package AND is a "user-source" symbol — declared in the package's
// own source rather than synthesized by the compiler. The predicate
// matches the share-gate in pe_implib.go's peLoadImportLibrary
// (excludes runtime.* / internal/* / go:* / type:* / go.*) plus an
// extra filter to skip compiler-generated symbols that share names
// with each other across plugins but must NOT be merged.
//
// Compiler-generated symbols are recognised by the `..` (consecutive
// dot) pattern Go uses for them: `pkg..inittask`, `pkg..stmp_N`,
// `pkg..autotmp_N`, etc. These are package-local internals — every
// plugin gets its own instance, and intra-package address-taking on
// them uses direct PC-rel, NOT @GOTPCREL. Merging them at the IAT
// level produces an inconsistent state where data writes go through
// __imp_ (to the canonical instance) but `&sym` evaluates to the local
// address, breaking init-time patterns like `var X = &stmp{...}`.
func isUserPackageName(name string) bool {
	if strings.HasPrefix(name, "runtime.") ||
		strings.HasPrefix(name, "internal/") ||
		strings.HasPrefix(name, "go:") ||
		strings.HasPrefix(name, "type:") ||
		strings.HasPrefix(name, "go.") {
		return false
	}
	if !strings.Contains(name, "/") {
		// Stdlib top-level package names have no slash. User packages
		// always have a module-path prefix with at least one slash.
		return false
	}
	if strings.Contains(name, "..") {
		// Compiler-generated symbol; skip.
		return false
	}
	return true
}

// userDataLink collects the per-module user-package DATA symbol tables
// that L3 uses for peer-plugin canonicalisation on platforms where the
// OS dynamic linker doesn't merge symbol namespaces (Windows).
//
// Two carriers are emitted, in parallel-array form mirroring the funcptr
// triplet:
//
//   - runtime.userDataExports{links,nameoffs,names}: every reachable
//     SDATA/SBSS/SNOPTRDATA/SNOPTRBSS user-package symbol this module
//     defines. links holds the symbol's resolved address; the runtime
//     registers (name → addr) in canonicalSyms with first-loader-wins
//     semantics so peer modules can find the canonical instance.
//
//   - runtime.userDataImports{links,nameoffs,names}: every reachable
//     `__imp_X` SNOPTRDATA wrapper synthesized by peGotRedirect for a
//     user-package data symbol. links holds the wrapper's address (the
//     8-byte slot whose content is initially &local_X); the runtime
//     rewrites the slot to point at the canonical owner's instance when
//     this module is not the first loader of name X.
//
// On POSIX no `__imp_` wrappers are synthesized so userDataImports is
// empty; the dynamic linker's GOT resolution already merges symbols
// across shlibs. The userDataExports walk still runs and registers
// entries in canonicalSyms, but the runtime's import-walk no-ops.
func (ctxt *Link) userDataLink() {
	ldr := ctxt.loader
	var exports, imports []loader.Sym
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		name := ldr.SymName(s)
		st := ldr.SymType(s)
		if strings.HasPrefix(name, "__imp_") {
			// Import slot — emitted by peGotRedirect for cross-module
			// data accesses. Underlying symbol name is name[6:].
			if st != sym.SNOPTRDATA {
				// SHOSTOBJ slots (host-shared, OS-loader-filled) don't
				// need rewriting — they already point at host's
				// canonical instance via standard PE Import Directory.
				continue
			}
			under := name[len("__imp_"):]
			if !isUserPackageName(under) {
				continue
			}
			imports = append(imports, s)
			continue
		}
		if !isUserPackageName(name) {
			continue
		}
		switch st {
		case sym.SDATA, sym.SBSS, sym.SNOPTRDATA, sym.SNOPTRBSS:
			exports = append(exports, s)
		}
	}
	slices.SortFunc(exports, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})
	slices.SortFunc(imports, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})

	ctxt.emitDataCarriers("userDataExports", exports, "")
	ctxt.emitDataCarriers("userDataImports", imports, "__imp_")
}

// emitDataCarriers emits the three carrier symbols for a user-data slot
// category (kind = "userDataExports" | "userDataImports"). namePrefix is
// stripped from each symbol name before being written to the names blob
// so the runtime can key both export and import entries on the bare
// underlying symbol name.
func (ctxt *Link) emitDataCarriers(kind string, slots []loader.Sym, namePrefix string) {
	ldr := ctxt.loader
	ptrsize := ctxt.Arch.PtrSize

	links := ldr.CreateSymForUpdate("runtime."+kind+"links", 0)
	links.SetType(sym.SRODATA)
	ldr.SetAttrLocal(links.Sym(), true)
	links.SetSize(int64(ptrsize * len(slots)))
	links.Grow(links.Size())
	relocs := links.AddRelocs(len(slots))
	for i, s := range slots {
		r := relocs.At(i)
		r.SetSym(s)
		r.SetOff(int32(i * ptrsize))
		r.SetSiz(uint8(ptrsize))
		r.SetType(objabi.R_ADDR)
	}

	nameoffs := ldr.CreateSymForUpdate("runtime."+kind+"nameoffs", 0)
	nameoffs.SetType(sym.SRODATA)
	ldr.SetAttrLocal(nameoffs.Sym(), true)
	nameoffs.SetSize(int64(4 * len(slots)))
	nameoffs.Grow(nameoffs.Size())

	names := ldr.CreateSymForUpdate("runtime."+kind+"names", 0)
	names.SetType(sym.SRODATA)
	ldr.SetAttrLocal(names.Sym(), true)

	off := int64(0)
	for i, s := range slots {
		full := ldr.SymName(s)
		target := strings.TrimPrefix(full, namePrefix)
		nameoffs.SetUint32(ctxt.Arch, int64(i*4), uint32(off))
		names.Addstring(target)
		off += int64(len(target)) + 1
	}
}

// emitSlotCarriers emits the three carrier symbols for a slot category
// (kind = "globalptr" or "funcptr"): runtime.<kind>links (slot
// addresses), runtime.<kind>nameoffs (per-slot int32 offsets), and
// runtime.<kind>names (packed null-terminated names).
func (ctxt *Link) emitSlotCarriers(kind string, slots []loader.Sym) {
	ldr := ctxt.loader
	ptrsize := ctxt.Arch.PtrSize
	prefix := "go." + kind + "."

	// runtime.<kind>links — slot addresses.
	links := ldr.CreateSymForUpdate("runtime."+kind+"links", 0)
	links.SetType(sym.SRODATA)
	ldr.SetAttrLocal(links.Sym(), true)
	links.SetSize(int64(ptrsize * len(slots)))
	links.Grow(links.Size())
	relocs := links.AddRelocs(len(slots))
	for i, s := range slots {
		r := relocs.At(i)
		r.SetSym(s)
		r.SetOff(int32(i * ptrsize))
		r.SetSiz(uint8(ptrsize))
		r.SetType(objabi.R_ADDR)
	}

	// runtime.<kind>nameoffs and runtime.<kind>names — extract target
	// name from each slot symbol's name (strip prefix), pack the names
	// into a single blob, record per-slot offsets in parallel.
	nameoffs := ldr.CreateSymForUpdate("runtime."+kind+"nameoffs", 0)
	nameoffs.SetType(sym.SRODATA)
	ldr.SetAttrLocal(nameoffs.Sym(), true)
	nameoffs.SetSize(int64(4 * len(slots)))
	nameoffs.Grow(nameoffs.Size())

	names := ldr.CreateSymForUpdate("runtime."+kind+"names", 0)
	names.SetType(sym.SRODATA)
	ldr.SetAttrLocal(names.Sym(), true)

	off := int64(0)
	for i, s := range slots {
		full := ldr.SymName(s)
		target := strings.TrimPrefix(full, prefix)
		// Write the int32 offset.
		nameoffs.SetUint32(ctxt.Arch, int64(i*4), uint32(off))
		// Addstring appends target AND a NUL terminator — don't add
		// a second NUL or offsets drift behind the blob's actual
		// position by one byte per slot.
		names.Addstring(target)
		off += int64(len(target)) + 1
	}
}
