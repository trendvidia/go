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

// staticWordRef identifies one pointer-sized word inside a static data
// symbol: the word lives at container+off, and its link-time content is
// the address of target (+add). Collected by staticCanonWords for the
// runtime's load-time canonicalisation passes.
type staticWordRef struct {
	container loader.Sym
	off       int64
	target    loader.Sym
	add       int64
}

// slotSymPrefixes are the three -typetab slot families the compiler
// emits under a "go." package prefix: go.typeptr.<T>
// (cmd/compile/internal/reflectdata/reflect.go), go.itabptr.<T,I>
// (same file) and go.funcptr.<F>
// (cmd/compile/internal/staticdata/globalptr.go). Every other
// compiler-synthesized symbol family uses a "go:" prefix instead —
// go:itab.T,I (gc/main.go sets ir.Pkgs.Itab.Prefix = "go:itab"),
// go:string.*, go:info.* — and go.buildid is STEXT, so neither reaches
// the SDATA/SNOPTRDATA/SRODATA scan below.
var slotSymPrefixes = [...]string{"go.typeptr.", "go.itabptr.", "go.funcptr."}

// skipStaticCanonContainer reports whether a static data symbol named
// name is excluded from the staticCanonWords scan:
//
//   - type: and go: descriptor/itab/string symbols, whose own R_ADDRs
//     feed typemap and itabTable population and must stay module-local;
//   - the go.<kind>. slot symbols and the runtime./internal/ carriers,
//     rewritten by their own load-time passes;
//   - __imp_ import slots, rewritten by userDataInit.
//
// The slot families are enumerated rather than matched with a blanket
// "go." prefix. "go." also prefixes every package hosted under a go.*
// module domain — go.uber.org/zap, go.opentelemetry.io/otel,
// go.etcd.io/bbolt, go.mongodb.org/mongo-driver — so a blanket match
// would silently drop exactly the static words this scan exists to
// canonicalize, with no diagnostic, for a large and common region of
// real plugin code.
func skipStaticCanonContainer(name string) bool {
	for _, p := range &slotSymPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return isToolchainOwnedName(name) || strings.HasPrefix(name, "__imp_")
}

// staticCanonWords scans reachable static data for pointer-sized R_ADDR
// words that embed a type descriptor, an itab, or a user-package data
// address — the words the -typetab slot machinery cannot reach, because
// no slot-routed code access ever rewrites them (trendvidia/go-archive#25):
// rtype/itab entries inside generic dictionaries, statically-initialized
// interface globals, type-assert descriptors, and `&pkg.Global` baked
// into initialized data. Each such word gets an entry in the same
// load-time rewrite tables the slots use (typeptrs/typeptroffs,
// itabptrs) or, for data addresses, the udaddr triplet, addressed as
// container+off instead of a dedicated slot symbol.
//
// Windows plugins only: PE has no symbol interposition, so a plugin's
// data words keep plugin-local addresses. On ELF and Mach-O the dynamic
// linker binds these words to the first-loaded definition (measured on
// darwin/arm64 and linux: the canonicaldata fixture passes unmodified),
// and rewriting them from typemap could REGRESS the corner where the
// OS loader bound a word whose type has no typelink entry.
//
// Containers scanned are SDATA/SNOPTRDATA/SRODATA, excluding descriptor
// and itab symbols themselves (their internal R_ADDRs feed typemap and
// itabTable population and must stay module-local), slot and carrier
// symbols, and runtime/internal packages (whose data the runtime may
// read before or during the rewrite passes). An SRODATA container that
// receives an entry is retyped to SNOPTRDATA so the runtime's rewrite
// lands in a writable PE section — safe, because read-only data can
// hold no heap pointers for the GC to trace.
//
// Targets that were originally TEXT are excluded: the linker already
// resolved words addressing a host-shared function to its PLT stub,
// and no `__imp_` slot holds a function address to rewrite them with.
func (ctxt *Link) staticCanonWords() (typeWords, itabWords, addrWords []staticWordRef) {
	if ctxt.BuildMode != BuildModePlugin || !ctxt.IsWindows() {
		return nil, nil, nil
	}
	ldr := ctxt.loader
	ptrsize := ctxt.Arch.PtrSize
	retype := make(map[loader.Sym]bool)
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		switch ldr.SymType(s) {
		case sym.SDATA, sym.SNOPTRDATA, sym.SRODATA:
		default:
			continue
		}
		if skipStaticCanonContainer(ldr.SymName(s)) {
			continue
		}
		relocs := ldr.Relocs(s)
		entries := 0
		for ri := 0; ri < relocs.Count(); ri++ {
			r := relocs.At(ri)
			if r.Type() != objabi.R_ADDR || int(r.Siz()) != ptrsize {
				continue
			}
			t := r.Sym()
			if t == 0 {
				continue
			}
			tname := ldr.SymName(t)
			switch {
			case r.Add() == 0 && strings.HasPrefix(tname, "type:") && !strings.HasPrefix(tname, "type:."):
				typeWords = append(typeWords, staticWordRef{s, int64(r.Off()), t, 0})
				entries++
			case r.Add() == 0 && ldr.IsItab(t):
				itabWords = append(itabWords, staticWordRef{s, int64(r.Off()), t, 0})
				entries++
			default:
				if !isUserPackageName(tname) {
					continue
				}
				// Data symbol classes as userDataLink registers them,
				// plus the two host-shared shapes a target takes by
				// this point: SHOSTOBJ (external link — the host
				// implib replaced the local definition) and SWINDOWS
				// (internal link — the .windynamic IAT slot).
				switch ldr.SymType(t) {
				case sym.SDATA, sym.SBSS, sym.SNOPTRDATA, sym.SNOPTRBSS,
					sym.SHOSTOBJ, sym.SWINDOWS:
				default:
					continue
				}
				// SHOSTOBJ and SWINDOWS say "host-shared", not
				// "data": peLoadImportLibrary converts host-shared
				// FUNCTIONS to those same types (it gates only data
				// symbols on the share list), so the two arms above
				// also admit text. A word addressing one is not a
				// data address and must not be rewritten: the linker
				// already resolved it to the callable PLT stub in
				// .rel (windynrelocsyms, plus relocsym's Windows-
				// plugin R_ADDR-to-SWINDOWS case in data.go), and
				// *(__imp_X) for a text X reads that stub's first
				// instruction bytes — so userDataAddrsInit would
				// store `ff 25 .. .. .. .. 90 90` where a function
				// address belongs and the next call through the word
				// would jump to garbage. The shape that reaches here
				// is a `pkg.Func·f` funcval container holding one
				// R_ADDR to a host-shared body: -buildmode=plugin
				// makes those SNOPTRDATA, hence scannable
				// (staticdata.WriteFuncSyms), where -buildmode=exe
				// leaves them SRODATA.
				//
				// FuncInfo survives peLoadImportLibrary's SetType
				// conversion, so it is the reliable "was this
				// originally a function?" test — the same one
				// data.go's stub loop uses to decide which SWINDOWS
				// symbols get a JMP thunk. Canonicalising static func
				// VALUES belongs to the funcptr carrier, not this one
				// (trendvidia/go-archive#106).
				if fi := ldr.FuncInfo(t); fi.Valid() {
					continue
				}
				// The canonical address of tname for this module is
				// *(__imp_tname) once the OS loader fills the IAT and
				// userDataInit rewrites peer wrappers; peGotRedirect
				// returns (or synthesizes) that slot for every case.
				// Synthesized wrappers are picked up by userDataLink's
				// walk below, so they join udimports and get both the
				// canonical rewrite and the Close dependency edge.
				slot := peGotRedirect(ctxt.Arch, ldr, t)
				addrWords = append(addrWords, staticWordRef{s, int64(r.Off()), slot, r.Add()})
				entries++
			}
		}
		if entries > 0 && ldr.SymType(s) == sym.SRODATA {
			retype[s] = true
		}
	}
	for s := range retype {
		ldr.MakeSymbolUpdater(s).SetType(sym.SNOPTRDATA)
	}
	return typeWords, itabWords, addrWords
}

// canonlink emits the fork's per-module canonicalisation tables: the
// typeptrs/typeptroffs and itabptrs slot tables the runtime rewrites at
// plugin load, plus the udaddr carrier triplet for static-data words
// that embed a user-package data address.
//
// It was called typelink, in a file called typelink.go, because it grew
// inside upstream's function of that name. go1.27 deleted upstream's
// half — the runtime.typelink and runtime.itablink tables (CL "cmd/link,
// runtime: remove typelinks" and "runtime, cmd/link: remove itablinks")
// — leaving nothing here that emits a typelink. reflect.typelinks now
// walks the type-descriptor range lazily and itabs are walked in place.
// The name went with the deleted half (trendvidia/go-archive#139).
func (ctxt *Link) canonlink() {
	ldr := ctxt.loader
	ptrsize := ctxt.Arch.PtrSize

	// Static-data words carrying type/itab/data addresses that no slot
	// covers (trendvidia/go-archive#25). Type and itab words join the slot
	// tables below as container+offset entries; data-address words get
	// the udaddr carrier triplet. Non-empty only for Windows plugins.
	staticTypeWords, staticItabWords, staticAddrWords := ctxt.staticCanonWords()
	ctxt.emitAddrCarriers(staticAddrWords)

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
		if !strings.HasPrefix(name, typeptrSlotPrefix) {
			continue
		}
		// Keep only a slot with the expected pointer-sized payload
		// and its R_ADDR relocation to the type. The seen-by-name guard
		// is defensive: loader.(*loadState).addSym returns the
		// EXISTING Sym for a DUPOK redefinition (without adding it to
		// objSyms), so one name yields one loader.Sym here, and
		// testplugin.TestTypetabLinkedTables asserts exactly that by
		// equating this count with the linked go.typeptr.* symbols.
		// That equality is about THIS slice, not about the emitted
		// runtime.typeptrs table: staticCanonWords appends static-data
		// word entries after the slots, so on a Windows plugin the
		// table is longer than the slot count. The test runs on ELF
		// only, where staticCanonWords returns nothing.
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

	// The slot entries are followed by the static-data word entries:
	// same parallel-table contract, but the rewritable word is
	// container+off rather than a dedicated go.typeptr slot.
	ntypeptrs := len(typeptrs) + len(staticTypeWords)
	tp := ldr.CreateSymForUpdate("runtime.typeptrs", 0)
	tp.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(tp.Sym(), true)
	tp.SetSize(int64(ptrsize * ntypeptrs))
	tp.Grow(tp.Size())
	if ntypeptrs > 0 {
		relocs := tp.AddRelocs(ntypeptrs)
		for i, s := range typeptrs {
			r := relocs.At(i)
			r.SetSym(s)
			r.SetOff(int32(i * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
		for i, w := range staticTypeWords {
			r := relocs.At(len(typeptrs) + i)
			r.SetSym(w.container)
			r.SetAdd(w.off)
			r.SetOff(int32((len(typeptrs) + i) * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
	}

	tpo := ldr.CreateSymForUpdate("runtime.typeptroffs", 0)
	tpo.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(tpo.Sym(), true)
	tpo.SetSize(int64(4 * ntypeptrs))
	tpo.Grow(tpo.Size())
	if ntypeptrs > 0 {
		relocs := tpo.AddRelocs(ntypeptrs)
		for i, s := range typeptrs {
			r := relocs.At(i)
			r.SetSym(typeptrTypes[s])
			r.SetOff(int32(i * 4))
			r.SetSiz(4)
			r.SetType(objabi.R_ADDROFF)
		}
		for i, w := range staticTypeWords {
			r := relocs.At(len(typeptrs) + i)
			r.SetSym(w.target)
			r.SetOff(int32((len(typeptrs) + i) * 4))
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
		if !strings.HasPrefix(name, itabptrSlotPrefix) {
			continue
		}
		// Keep only a slot with the expected pointer-sized payload
		// and its R_ADDR relocation to the itab. The seen-by-name guard
		// is defensive: loader.(*loadState).addSym returns the
		// EXISTING Sym for a DUPOK redefinition (without adding it to
		// objSyms), so one name yields one loader.Sym here, and
		// testplugin.TestTypetabLinkedTables asserts exactly that by
		// equating this count with the linked go.itabptr.* symbols.
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

	// As with typeptrs: static-data itab words follow the slot entries,
	// addressed as container+off.
	nitabptrs := len(itabptrs) + len(staticItabWords)
	ip := ldr.CreateSymForUpdate("runtime.itabptrs", 0)
	ip.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(ip.Sym(), true)
	ip.SetSize(int64(ptrsize * nitabptrs))
	ip.Grow(ip.Size())
	if nitabptrs > 0 {
		relocs := ip.AddRelocs(nitabptrs)
		for i, s := range itabptrs {
			r := relocs.At(i)
			r.SetSym(s)
			r.SetOff(int32(i * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
		for i, w := range staticItabWords {
			r := relocs.At(len(itabptrs) + i)
			r.SetSym(w.container)
			r.SetAdd(w.off)
			r.SetOff(int32((len(itabptrs) + i) * ptrsize))
			r.SetSiz(uint8(ptrsize))
			r.SetType(objabi.R_ADDR)
		}
	}
}

// emitAddrCarriers emits the udaddr carrier triplet for static-data
// words that embed a user-package data address (trendvidia/go-archive#25, the
// `var P = &pkg.Global` shape):
//
//   - runtime.udaddrwords — address of each word to rewrite
//     (container+off R_ADDR).
//   - runtime.udaddrslots — address of the matching `__imp_X` slot,
//     whose content is X's canonical address once the OS loader has
//     filled the IAT (host-shared X) and userDataInit has rewritten
//     peer wrappers (peer-owned X).
//   - runtime.udaddradds — per-word addend (`&pkg.Arr[2]` embeds
//     &pkg.Arr + 16).
//
// The runtime's userDataAddrsInit walks the triplet at plugin load and
// stores *slot+add into each word. Carriers are empty except for
// Windows plugins.
func (ctxt *Link) emitAddrCarriers(addrWords []staticWordRef) {
	ldr := ctxt.loader
	ptrsize := ctxt.Arch.PtrSize

	words := ldr.CreateSymForUpdate("runtime.udaddrwords", 0)
	words.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(words.Sym(), true)
	words.SetSize(int64(ptrsize * len(addrWords)))
	words.Grow(words.Size())

	slots := ldr.CreateSymForUpdate("runtime.udaddrslots", 0)
	slots.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(slots.Sym(), true)
	slots.SetSize(int64(ptrsize * len(addrWords)))
	slots.Grow(slots.Size())

	adds := ldr.CreateSymForUpdate("runtime.udaddradds", 0)
	adds.SetType(sym.SNOPTRDATA)
	ldr.SetAttrLocal(adds.Sym(), true)
	adds.SetSize(int64(ptrsize * len(addrWords)))
	adds.Grow(adds.Size())

	if len(addrWords) == 0 {
		return
	}
	wrelocs := words.AddRelocs(len(addrWords))
	srelocs := slots.AddRelocs(len(addrWords))
	for i, w := range addrWords {
		r := wrelocs.At(i)
		r.SetSym(w.container)
		r.SetAdd(w.off)
		r.SetOff(int32(i * ptrsize))
		r.SetSiz(uint8(ptrsize))
		r.SetType(objabi.R_ADDR)

		r = srelocs.At(i)
		r.SetSym(w.target)
		r.SetOff(int32(i * ptrsize))
		r.SetSiz(uint8(ptrsize))
		r.SetType(objabi.R_ADDR)

		adds.SetUint(ctxt.Arch, int64(i*ptrsize), uint64(w.add))
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
		if strings.HasPrefix(name, funcptrSlotPrefix) {
			funcptrs = append(funcptrs, s)
		}
	}
	slices.SortFunc(funcptrs, func(a, b loader.Sym) int {
		return strings.Compare(ldr.SymName(a), ldr.SymName(b))
	})

	ctxt.emitSlotCarriers("funcptr", funcptrs)
}

// Prefixes of the per-module indirection-slot families. The collectors
// that gather each family (typeptrlink, itabptrlink, funcptrlink) match
// on these constants and isModuleSlotName is defined over the same
// list, so the emitters and the two gates that must exclude the slots
// (userDataLink's isUserPackageName, pe_implib.go's peSkipSymName)
// cannot drift apart: a fourth family is one edit here away from being
// excluded everywhere. There WAS a fourth — `go.globalptr.`, removed in
// the Bug D fix (see funcptrlink above) — so this list is not
// hypothetically extensible, it has already changed once.
const (
	typeptrSlotPrefix = "go.typeptr."
	itabptrSlotPrefix = "go.itabptr."
	funcptrSlotPrefix = "go.funcptr."
)

var moduleSlotPrefixes = []string{typeptrSlotPrefix, itabptrSlotPrefix, funcptrSlotPrefix}

// isModuleSlotName reports whether name is one of the three per-module
// indirection-slot families emitted under -typetab / -dynlink:
// go.typeptr.<T>, go.itabptr.<X>, go.funcptr.<F>. Each module gets its
// own slots and the runtime's typeptrsinit / itabptrsinit /
// funcptrsinit rewrite the slot CONTENTS at plugin load, so the slots
// themselves must never be merged, exported, or IAT-routed across
// modules.
//
// These three families are the only `go.`-prefixed names the toolchain
// emits with a dot-and-slash shape; `go.` is otherwise a legal module
// domain (go.uber.org, go.opentelemetry.io, go.etcd.io, ...), so gates
// must match these families exactly and never the bare `go.` prefix —
// a blanket prefix test would exclude every package hosted under such
// a domain from cross-module data canonicalisation (trendvidia/go-archive#108).
// A domain only survives into a symbol name unescaped when it is not
// the last path element: objabi.PathToPrefix escapes dots after the
// final slash, so package go.uber.org/zap's globals are
// `go.uber.org/zap.X` but a root package of `module go.example.org`
// is `go%2eexample%2eorg.X`, which no `go.` test sees at all.
func isModuleSlotName(name string) bool {
	for _, prefix := range moduleSlotPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// isInternalPkgName reports whether name's package is an internal
// package — one whose import path has "internal" as a path element, so
// only the tree containing it may import it.
//
// This is the Go rule, not a prefix: crypto/internal/fips140/bigmod is
// as internal as internal/abi is. Both predicates below used to test
// strings.HasPrefix(name, "internal/"), which catches only the
// top-level tree and admits every nested one, so the whole
// crypto/internal/... subtree was treated as user code and entered
// cross-module canonicalisation (trendvidia/go-archive#111).
//
// The three arms are the three positions the element can occupy: first
// ("internal/abi.Kind"), middle ("crypto/internal/fips140/bigmod.X")
// and last, where the package itself is named internal and the symbol's
// dot follows it directly ("golang.org/x/tools/internal.Version").
// Requiring the slash is what keeps "mypkg/notinternal/x.Y" and
// "github.com/internal-tools/x.Y" out — neither has such an element.
//
// A version-suffixed path is why this cannot be done by splitting on
// dots: crypto/internal/fips140/v1.26.0/bigmod.addMulVVW1024 carries
// dots in a path element, not only in the symbol.
func isInternalPkgName(name string) bool {
	return strings.HasPrefix(name, "internal/") ||
		strings.Contains(name, "/internal/") ||
		strings.Contains(name, "/internal.")
}

// isToolchainOwnedName reports whether name belongs to the runtime, to
// one of the toolchain's own symbol namespaces, or to an internal
// package — the names cross-module data canonicalisation must leave
// module-local.
//
// One definition, because this scoping rule existed in copies and had
// been wrong in a different direction in each (trendvidia/go-archive#111, and
// #108/#110/#115 before it). isUserPackageName gates what gets
// REGISTERED for canonicalisation and skipStaticCanonContainer gates
// which static containers get SCANNED for words to rewrite: the two ask
// the same question about a name and now get the same answer, while
// keeping the arms that are genuinely their own (module slots for the
// first, `__imp_` and the slot prefixes for the second).
//
// Distinct from peShareHostData in pe_implib.go, which deliberately does
// NOT exclude internal packages: PE symbol sharing gives host and plugin
// one instance of a symbol at link time (trendvidia/go-archive#74), while this
// governs a runtime rewrite over data the runtime may itself be reading
// while the rewrite runs.
func isToolchainOwnedName(name string) bool {
	return strings.HasPrefix(name, "runtime.") ||
		strings.HasPrefix(name, "go:") ||
		strings.HasPrefix(name, "type:") ||
		isInternalPkgName(name)
}

// isUserPackageName reports whether name is a Go package symbol that
// may be canonicalised across modules — declared in a package's own
// source rather than synthesized by the compiler, and not owned by a
// subsystem that requires a per-module instance.
//
// It is an EXCLUSION list, deliberately. The rule it implements is the
// one POSIX gets from the dynamic linker for free: identically-named
// package symbols are one object across the host and every plugin,
// unless something specific requires otherwise. Those exceptions are:
//
//   - runtime.*, whose singletons are shared through the curated
//     peSharedRuntimeData list instead, because some must stay local;
//   - internal/*, likewise shared by peLoadImportLibrary's gate when
//     the host links the package (trendvidia/go-archive#74);
//   - go:* and type:*, linker-internal carriers and type descriptors,
//     per-module by design;
//   - the three go.<kind>. slot families, which the runtime rewrites
//     per module (isModuleSlotName);
//   - compiler-generated symbols, recognised by the `..` pattern Go
//     uses for them: `pkg..inittask`, `pkg..stmp_N`, `pkg..autotmp_N`.
//     These are package-local internals — every plugin gets its own,
//     and intra-package address-taking on them uses direct PC-rel, NOT
//     @GOTPCREL. Merging them at the IAT level produces an inconsistent
//     state where data writes go through __imp_ (to the canonical
//     instance) but `&sym` evaluates to the local address, breaking
//     init-time patterns like `var X = &stmp{...}`.
//
// It used to carry a sixth exclusion: names with no slash, read as
// "stdlib top-level package". That was a heuristic standing in for a
// fact the linker does not have — nothing here can tell `os.Args` from
// `extensions.Counter`, the root package of a module named without a
// domain, and `sym.Library` carries no GOROOT marker to ask. It cost
// correctness in both directions (trendvidia/go-archive#115): a package at a
// dotless module root kept one instance per module, and so did every
// slashless STDLIB package that plugins link and the host does not —
// two plugins importing net measured different `net.DefaultResolver`
// where POSIX gives them one. Dropping it makes Windows agree with
// POSIX and removes the guess; the exclusions above are all stated as
// facts about specific subsystems rather than inferred from a name's
// shape.
//
// peSkipSymName, pe_implib.go's share-gate, is still the wider of the
// two: it carves `pkg..inittask` out of its `..` filter so a host can
// publish init tasks for peLoadImportLibrary to bind (trendvidia/go-archive#61),
// while an inittask is per-module layout and must never be registered
// for canonicalisation. TestModuleSlotNameLockstep pins where the two
// must agree; TestGateAsymmetryOutsideSlotFamilies pins what is left of
// the divergence.
func isUserPackageName(name string) bool {
	if isToolchainOwnedName(name) || isModuleSlotName(name) {
		return false
	}
	if strings.Contains(name, "..") {
		// Compiler-generated symbol; skip.
		return false
	}
	if i := strings.IndexByte(name, '.'); i <= 0 {
		// Not a Go package-level symbol. A Go global is always
		// `pkg.Name`, so a name with no package prefix belongs to
		// something else: C and cgo globals (`_cgo_init`,
		// `_crosscall2_ptr`, `timebase`, `block_size`), linker
		// artifacts (`.rel`), and the thousands of ANONYMOUS symbols a
		// link carries — measured, 7829 empty names in one
		// testplugin test binary. Those have no section, so a
		// relocation from the export carrier to one fails the link
		// outright ("missing section for relocation target").
		//
		// The old "must contain a slash" arm hid this: requiring a
		// module-path prefix also required a package prefix. Dropping
		// it for trendvidia/go-archive#115 separated the two, so the part that
		// was doing real work is stated here on its own.
		return false
	}
	return true
}

// isFuncValName reports whether name is a compiler-emitted funcval
// symbol — the one-word object a func VALUE points at, holding the
// function's ABIInternal entry address. staticdata.WriteFuncSyms emits
// one per function used as a value rather than called directly, named
// ir.FuncSymName(f), which is f + "\u00b7f".
func isFuncValName(name string) bool {
	return strings.HasSuffix(name, "\u00b7f")
}

// userDataExportable reports whether a reachable symbol named name with
// symbol kind st belongs in runtime.userDataExports — the registry the
// runtime canonicalises cross-module references against.
//
// The four writable data kinds are the original set: a package-level
// global lives in one of them, and canonicalisation exists to give
// every module one instance of it.
//
// SRODATA is admitted for funcval symbols ONLY (trendvidia/go-archive#106).
// A funcval is read-only in an exe and writable under -dynlink
// (WriteFuncSyms marks it NOPTR there so Plugin.Close can overwrite the
// code pointer), and that symbol-KIND asymmetry is exactly what used to
// exclude a plugin HOST's funcvals from this walk while including every
// plugin's. Nothing then canonicalised a plugin's funcval against the
// host's, so a func value the plugin materialised for a host-shared
// function pointed at the plugin's own `·f`, whose word is the address
// of the plugin's IAT thunk. Measured on windows/{amd64,arm64}:
// reflect.ValueOf(shared.Fn).Pointer() disagreed with the host's, and
// runtime.FuncForPC named the value ".rel" — the linker's thunk table —
// where POSIX names the function. Registering the host's funcvals feeds
// BOTH consumers of canonicalSyms, since canonicalizeSlots keys the
// funcptr carrier on the funcval's own name and userDataInit keys the
// data walk the same way.
//
// The kind is narrowed to funcvals rather than opened to SRODATA: the
// rest of that section is type descriptors, strings and the slot
// carriers, which are per-module by design and which isUserPackageName
// already rejects by name. Both filters are pinned by
// TestUserDataExportable.
func userDataExportable(name string, st sym.SymKind) bool {
	if !isUserPackageName(name) {
		return false
	}
	switch st {
	case sym.SDATA, sym.SBSS, sym.SNOPTRDATA, sym.SNOPTRBSS:
		return true
	case sym.SRODATA:
		return isFuncValName(name)
	}
	return false
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
		if userDataExportable(name, st) {
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
// (kind = "funcptr"; "globalptr" was a second category until the Bug D
// fix removed those slots): runtime.<kind>links (slot addresses),
// runtime.<kind>nameoffs (per-slot int32 offsets), and
// runtime.<kind>names (packed null-terminated names). kind must be the
// middle component of one of moduleSlotPrefixes — the slot names it
// trims are "go."+kind+"." — which TestModuleSlotPrefixesCoverGates
// pins.
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
