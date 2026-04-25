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
}

// globalptrlink gathers all per-module go.globalptr.* and go.funcptr.*
// indirection slots emitted by the compiler under -shared/-dynlink and
// builds three carrier symbols per category:
//
//   - runtime.globalptrlinks / runtime.funcptrlinks — pointer-sized
//     slot addresses (R_ADDR relocations to each slot).
//   - runtime.globalptrnameoffs / runtime.funcptrnameoffs — int32
//     offsets into the per-category names blob, one per slot.
//   - runtime.globalptrnames / runtime.funcptrnames — packed null-
//     terminated target names (extracted from the slot symbol's name
//     by stripping the go.globalptr. or go.funcptr. prefix).
//
// moduledata exposes the three carriers as parallel slices so the
// runtime's canonicalisation pass can map each slot to its target's
// name and look up the canonical (first-loader-wins) instance across
// all loaded modules.
//
// In -buildmode=exe the compiler emits no slot symbols, so all
// carriers remain zero-sized and runtime canonicalisation is a no-op.
func (ctxt *Link) globalptrlink() {
	ldr := ctxt.loader
	var globalptrs, funcptrs []loader.Sym
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		name := ldr.SymName(s)
		switch {
		case strings.HasPrefix(name, "go.globalptr."):
			globalptrs = append(globalptrs, s)
		case strings.HasPrefix(name, "go.funcptr."):
			funcptrs = append(funcptrs, s)
		}
	}
	slices.SortFunc(globalptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})
	slices.SortFunc(funcptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})

	ctxt.emitSlotCarriers("globalptr", globalptrs)
	ctxt.emitSlotCarriers("funcptr", funcptrs)
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
		// Append target + NUL to names blob.
		names.Addstring(target)
		names.AddUint8(0)
		off += int64(len(target)) + 1
	}
}
