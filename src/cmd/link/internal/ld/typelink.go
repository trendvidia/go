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
// builds the runtime.globalptrlinks / runtime.funcptrlinks carrier
// symbols. moduledata exposes those carriers as slices so the runtime's
// canonicalisation pass at module-load time can iterate every slot
// without scanning the symbol table.
//
// In -buildmode=exe the compiler emits no slot symbols, so both
// carriers remain zero-sized and the corresponding moduledata slices
// are empty — runtime canonicalisation walks become no-ops.
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
	// Sort by symbol name so emission is deterministic across builds.
	slices.SortFunc(globalptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})
	slices.SortFunc(funcptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})

	ptrsize := ctxt.Arch.PtrSize
	emit := func(carrierName string, slots []loader.Sym) {
		c := ldr.CreateSymForUpdate(carrierName, 0)
		c.SetType(sym.SRODATA)
		ldr.SetAttrLocal(c.Sym(), true)
		c.SetSize(int64(ptrsize * len(slots)))
		c.Grow(c.Size())
		relocs := c.AddRelocs(len(slots))
		for i, s := range slots {
			r := relocs.At(i)
			r.SetSym(s)
			r.SetOff(int32(i * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
	}
	emit("runtime.globalptrlinks", globalptrs)
	emit("runtime.funcptrlinks", funcptrs)
}
