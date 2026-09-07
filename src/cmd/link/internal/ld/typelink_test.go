// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"strings"
	"testing"
)

// TestIsUserPackageName pins which symbols the Windows cross-module data
// canonicalisation machinery registers (userDataLink's export/import
// tables). The `go.`-prefixed arm must reject exactly the three
// per-module slot families and nothing else: `go.` is also a module
// domain (go.uber.org, go.opentelemetry.io, ...), and a blanket prefix
// test silently excluded every package hosted under one from
// canonicalisation — host and plugin each kept their own instance of
// such a package's globals (trendvidia/go#108).
//
// Names here are spelled the way the toolchain emits them. In
// particular the package part is objabi.PathToPrefix output, which
// escapes dots in the LAST path element: package gopkg.in/yaml.v2's
// globals are named `gopkg.in/yaml%2ev2.X`, never `gopkg.in/yaml.v2.X`.
// Slot names are not: itabptr embeds LinkStrings verbatim
// (`go.itabptr.*os.File,io.Reader`), typeptr embeds the type symbol
// name (`go.typeptr.type:*fmt.pp`). Measured with `go tool nm` on a
// -buildmode=plugin -gcflags=all=-typetab build.
func TestIsUserPackageName(t *testing.T) {
	for _, name := range []string{
		"testplugin/common.X",
		"go.uber.org/zap.Global",
		"go.opentelemetry.io/otel.Tracer",
		"go.example.org/fakemod/lib.Counter",
		"gopkg.in/yaml%2ev2.DefaultMapType",
		// Packages at the ROOT of a dotted module path
		// (trendvidia/go#110). PathToPrefix escapes dots after the
		// final slash, so when the whole import path is the last
		// element the name carries no slash at all and the "no slash
		// means stdlib" arm used to reject it — host and plugin then
		// kept separate instances of the global on Windows. The
		// rootmod fixture in cmd/cgo/internal/testplugin is the
		// end-to-end half of this pin.
		"example%2ecom.Counter",
		"go%2eexample%2eorg.Counter",
		"gopkg%2ein.Setting",
		// The escape is what admits it, not the leading `go`/dot: a
		// funcval or method-value symbol of the same root package is
		// still that package's.
		"example%2ecom.Fn·f",
	} {
		if !isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = false, want true: excluded from cross-module data canonicalisation", name)
		}
	}
	for _, name := range []string{
		"runtime.writeBarrier",
		"internal/bytealg.MaxLen",
		"go:itab.*os.File,io.Reader",
		"type:go.uber.org/zap.Logger",
		"go.typeptr.type:*go.uber.org/zap.Logger",
		"go.itabptr.*os.File,io.Reader",
		"go.funcptr.go.uber.org/zap.New",
		"strconv.isPrint16", // no slash: stdlib top-level
		"os.Args",           // ditto, and the widened arm must not reach it
		"main.someGlobal",   // the main package is slashless too
		"testplugin/common..stmp_1",
		"example%2ecom..inittask", // root-package inittask: still per-module
		"example%2ecom..stmp_1",
		"type:example%2ecom.T", // descriptor of a root-package type
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: must stay module-local", name)
		}
	}
	// Known gap, pinned rather than fixed (trendvidia/go#115): a module
	// path that is a single element with NO dot (`module testplugin`,
	// `module foo`) has nothing for PathToPrefix to escape, so a
	// package at its root is spelled exactly like a stdlib top-level
	// package and the two cannot be told apart from the symbol name.
	// The name is the only input both callers have — userDataLink's
	// import half derives it from an `__imp_X` wrapper whose target
	// need not be a symbol of this module — so the slashless arm
	// rejects both and such a root package keeps one instance per
	// module. #110's fix covers dotted module paths, which is every
	// path that names a domain.
	for _, name := range []string{
		"testplugin.RootGlobal",
		"foo.Counter",
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true: #115 fixed? update this pin and the comment above it", name)
		}
	}
}

// TestModuleSlotNameLockstep pins the agreement between userDataLink's
// registration gate (isUserPackageName) and peLoadImportLibrary's
// share-gate (peSkipSymName) on the two regions where they must not
// drift apart: the three per-module slot families, which BOTH must
// reject, and go.*-domain package symbols, which BOTH must admit. A
// slot family that one gate stops matching becomes a symbol registered
// for canonicalisation but skipped from the host's export table (or
// vice versa), which desynchronises the two halves of the L3 data
// canonicalisation subsystem.
//
// The two gates are NOT equivalent outside those regions and are not
// meant to be — peSkipSymName is the wider of the two. See
// TestGateAsymmetryOutsideSlotFamilies for the deliberate divergences.
func TestModuleSlotNameLockstep(t *testing.T) {
	for _, name := range []string{
		"go.typeptr.type:*go.uber.org/zap.Logger",
		"go.itabptr.*os.File,io.Reader",
		"go.funcptr.go.uber.org/zap.New",
	} {
		if !isModuleSlotName(name) {
			t.Errorf("isModuleSlotName(%q) = false, want true", name)
		}
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: per-module slot must not be registered for canonicalisation", name)
		}
		if !peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = false, want true: per-module slot must stay out of export tables and import libraries", name)
		}
	}
	for _, name := range []string{
		"go.uber.org/zap.Global",
		"go.example.org/fakemod/lib.Counter",
		// Root package of a dotted module path (trendvidia/go#110):
		// both gates must admit it, and for the same reason as the
		// go.*-domain names above — the rootmod fixture's host-shared
		// half (rootorg.Value) needs the export table AND the
		// canonicalisation registry to agree about it.
		"example%2ecom.Counter",
		"go%2eexample%2eorg.Counter",
	} {
		if isModuleSlotName(name) {
			t.Errorf("isModuleSlotName(%q) = true, want false", name)
		}
		if !isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = false, want true: user global must be registered for canonicalisation", name)
		}
		if peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = true, want false: user global must be exportable for host-plugin sharing", name)
		}
	}
}

// TestGateAsymmetryOutsideSlotFamilies pins the places where
// peSkipSymName admits a name that isUserPackageName rejects, so that
// nobody reading TestModuleSlotNameLockstep mistakes its two regions
// for a biconditional and "restores" a lockstep that was never there.
//
// Each of these is deliberate:
//
//   - `pkg..inittask`: peSkipSymName carves it out of the `..` filter so
//     a plugin HOST can publish its init tasks and peLoadImportLibrary
//     can bind them, keeping a plugin from re-running an init the host
//     already ran (trendvidia/go#61). Registering one for cross-module
//     canonicalisation would be wrong — an inittask is a layout-specific
//     per-module structure — so isUserPackageName rejects it. Adding the
//     inittask exemption to isUserPackageName is the mistake this case
//     exists to catch.
//   - stdlib top-level names (`os.Args`, `strconv.isPrint16`): exportable
//     from a host, but isUserPackageName's slashless arm keeps them out
//     of the canonicalisation registry; on Windows the host's import
//     library is what shares them.
//   - root packages of DOTLESS module paths (`testplugin.RootGlobal`):
//     same shape as the stdlib names and rejected for the same reason,
//     but unlike them the rejection is a known gap rather than a
//     decision — nothing in the symbol name separates the two cases.
//     trendvidia/go#115; the dotted paths #110 fixed have moved out of
//     this list and into TestModuleSlotNameLockstep's admit region.
func TestGateAsymmetryOutsideSlotFamilies(t *testing.T) {
	for _, name := range []string{
		"testplugin/common..inittask",
		"sync..inittask",
		"example%2ecom..inittask",
		"os.Args",
		"strconv.isPrint16",
		"testplugin.RootGlobal",
	} {
		if peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = true, want false: host must be able to export it", name)
		}
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: not a cross-module canonicalisation candidate", name)
		}
	}
}

// TestModuleSlotPrefixesCoverGates pins the assumption isModuleSlotName's
// doc states and that nothing else in the tree checks: the slot families
// the collectors GATHER are exactly the families the two gates EXCLUDE.
// The other tests here enumerate names by hand, so they can only fail for
// a family somebody remembered to add to them; this one is driven by
// moduleSlotPrefixes, the list typeptrlink / itabptrlink / funcptrlink
// match on, so a family added to the emitters and forgotten in a gate
// fails here instead of silently entering cross-module canonicalisation.
// That is not hypothetical: `go.globalptr.` was a fourth family until the
// Bug D fix removed it.
func TestModuleSlotPrefixesCoverGates(t *testing.T) {
	for _, prefix := range moduleSlotPrefixes {
		name := prefix + "p.X"
		if !isModuleSlotName(name) {
			t.Errorf("isModuleSlotName(%q) = false, want true", name)
		}
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: slot family %q entered the canonicalisation registry", name, prefix)
		}
		if !peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = false, want true: slot family %q entered an export table or import library", name, prefix)
		}
		// emitSlotCarriers builds each carrier's name as
		// "runtime.<kind>{links,nameoffs,names}" where <kind> is this
		// prefix with its "go." head and trailing dot stripped, and
		// recovers a slot's target name by trimming "go."+kind+"."
		// back off. Both directions require the `go.<kind>.` shape.
		kind := strings.TrimSuffix(strings.TrimPrefix(prefix, "go."), ".")
		if got := "go." + kind + "."; got != prefix {
			t.Errorf("slot prefix %q is not of the form go.<kind>.; emitSlotCarriers would build carriers for %q", prefix, got)
		}
	}
	// A `go.`-domain module path must not be reachable by extending any
	// family prefix — i.e. no prefix may be a prefix of a plausible
	// module domain. Cheap guard against a future family named, say,
	// "go.u" (which would swallow go.uber.org).
	for _, domain := range []string{"go.uber.org/zap.Global", "go.opentelemetry.io/otel.Tracer"} {
		if isModuleSlotName(domain) {
			t.Errorf("isModuleSlotName(%q) = true: a slot family prefix now swallows a module domain (trendvidia/go#108)", domain)
		}
	}
}

// TestSkipStaticCanonContainer pins the container exclusion list of the
// staticCanonWords scan (trendvidia/go#25). The scan is Windows-plugin
// only, so no fixture on any other platform reaches it; this exercises
// the predicate directly.
//
// The case that matters is the last group: a blanket "go." prefix — the
// shape the exclusion list started out with — also matches every package
// hosted under a go.* module domain, which would silently drop the very
// static words the scan exists to canonicalize.
func TestSkipStaticCanonContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip bool
	}{
		// Descriptor / compiler-synthesized symbol families.
		{"type:main.T", true},
		{"type:.namedata.Foo-", true},
		{"go:itab.main.T,main.I", true},
		{"go:string.\"hello\"", true},
		// -typetab slot families and load-time carriers.
		{"go.typeptr.main.T", true},
		{"go.itabptr.main.T,main.I", true},
		{"go.funcptr.main.F", true},
		{"go.typeptr.go.uber.org/zap.Logger", true},
		{"runtime.typeptrs", true},
		{"runtime.udaddrwords", true},
		{"internal/abi.Kind", true},
		{"__imp_example.com/m/pkg.Global", true},

		// Ordinary containers the scan must look inside.
		{"main.StaticErr", false},
		{"example.com/m/pkg.Table", false},
		{"main..dict.boxAny[main.T]", false},
		{"net/http.statusText", false},

		// Packages on go.* module domains: NOT slot symbols. A blanket
		// "go." prefix would exclude every one of these.
		{"go.uber.org/zap.Global", false},
		{"go.opentelemetry.io/otel.ErrHandler", false},
		{"go.etcd.io/bbolt.DefaultOptions", false},
		{"go.mongodb.org/mongo-driver/bson.ErrNilReader", false},
		{"go.opencensus.io/trace.defaultSampler", false},
		// A user package literally named "go.typeptr" would collide,
		// but a module path needs a dot-bearing domain and a slash;
		// this one is a package, not a slot, and must be scanned.
		{"go.typeptrs.io/x.Global", false},
	} {
		if got := skipStaticCanonContainer(tc.name); got != tc.skip {
			t.Errorf("skipStaticCanonContainer(%q) = %v, want %v", tc.name, got, tc.skip)
		}
	}
}
