// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/link/internal/sym"
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
// such a package's globals (trendvidia/go-archive#108).
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
		// (trendvidia/go-archive#110). PathToPrefix escapes dots after the
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
		// Slashless names (trendvidia/go-archive#115). The gate used to read a
		// missing slash as "stdlib top-level package" and reject it,
		// which cost correctness in both directions: a package at the
		// root of a module named without a domain (`go mod init
		// extensions`) kept one instance per module, and so did every
		// stdlib package that plugins link and the host does not.
		// There is no way to tell the two apart from the name, and no
		// GOROOT marker in sym.Library to ask instead, so the arm is
		// gone and both are canonicalised — which is what POSIX does.
		"extensions.Counter",
		"testplugin.RootGlobal",
		"os.Args",
		"strconv.isPrint16",
		"net.DefaultResolver",
		"main.someGlobal",
		"os.Exit·f", // a stdlib funcval, for the same reason
		// Near-misses for the nested-internal arm (trendvidia/go-archive#111):
		// "internal" has to be a whole path element, so a package
		// merely SPELLED with it is ordinary user code.
		"mypkg/notinternal/x.Y",
		"github.com/internal-tools/cli.Verbose",
		"example.com/m/internalish.Global",
		"example.com/m/pkg.InternalCounter",
	} {
		if !isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = false, want true: excluded from cross-module data canonicalisation", name)
		}
	}
	for _, name := range []string{
		"runtime.writeBarrier",
		"internal/bytealg.MaxLen",
		// Nested internal packages (trendvidia/go-archive#111). "internal" is
		// a path ELEMENT in Go's rule, not a prefix, and the gate used
		// to test only the top-level tree — so the whole
		// crypto/internal/... subtree was admitted as user code.
		"crypto/internal/fips140/bigmod.addMulVVW1024",
		"crypto/internal/fips140/v1.26.0/bigmod.addMulVVW1024",
		"crypto/internal/boring.ErrOpen",
		"golang.org/x/tools/internal/event.Err",
		// The element can also be last, with the symbol's dot right
		// after it: the package itself is named internal.
		"golang.org/x/tools/internal.Version",
		"go:itab.*os.File,io.Reader",
		"type:go.uber.org/zap.Logger",
		"go.typeptr.type:*go.uber.org/zap.Logger",
		"go.itabptr.*os.File,io.Reader",
		"go.funcptr.go.uber.org/zap.New",
		"testplugin/common..stmp_1",
		"example%2ecom..inittask", // root-package inittask: still per-module
		"example%2ecom..stmp_1",
		"type:example%2ecom.T", // descriptor of a root-package type
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: must stay module-local", name)
		}
	}
	// A name with no package prefix is not a Go package symbol, and
	// the old "must contain a slash" arm was quietly enforcing that
	// too. Dropping it admitted C and cgo globals and the thousands of
	// ANONYMOUS symbols a link carries — 7829 empty names in one
	// testplugin test binary — whose relocations from the export
	// carrier have no section and fail the link outright.
	for _, name := range []string{
		"",
		"_cgo_init",
		"_crosscall2_ptr",
		"timebase",
		"block_size",
		".rel",
		"main", // a bare package name is not a symbol
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: not a Go package-level symbol", name)
		}
	}

	// The exclusions still apply to slashless names — dropping the
	// "no slash" arm widened the gate, it did not remove the arms that
	// state actual facts about a subsystem.
	for _, name := range []string{
		"runtime.mheap_",
		"internal/poll.CloseFunc",
		"go:buildinfo",
		"type:os.File",
		"os..inittask",
		"sync..stmp_3",
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: #115 widened the slashless case, "+
				"it must not have widened the subsystem exclusions", name)
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
		// Root package of a dotted module path (trendvidia/go-archive#110):
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
//     already ran (trendvidia/go-archive#61). Registering one for cross-module
//     canonicalisation would be wrong — an inittask is a layout-specific
//     per-module structure — so isUserPackageName rejects it. Adding the
//     inittask exemption to isUserPackageName is the mistake this case
//     exists to catch.
//
// The `..inittask` carve-out is the ONLY divergence left. Slashless
// names used to be the other one — stdlib top-level names and root
// packages of dotless module paths, which peSkipSymName admitted and
// isUserPackageName rejected — and that asymmetry was a heuristic, not
// a decision: nothing separates `os.Args` from `extensions.Counter` by
// name. Dropping it (trendvidia/go-archive#115) made both gates agree, so those
// cases moved to TestIsUserPackageName's admit loop.
func TestGateAsymmetryOutsideSlotFamilies(t *testing.T) {
	for _, name := range []string{
		"testplugin/common..inittask",
		"sync..inittask",
		"example%2ecom..inittask",
		"os..inittask",
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
			t.Errorf("isModuleSlotName(%q) = true: a slot family prefix now swallows a module domain (trendvidia/go-archive#108)", domain)
		}
	}
}

// TestSkipStaticCanonContainer pins the container exclusion list of the
// staticCanonWords scan (trendvidia/go-archive#25). The scan is Windows-plugin
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
		// Nested internal packages, the second copy of the rule
		// (trendvidia/go-archive#111). Both predicates now share one
		// definition, so these move in lockstep with the
		// isUserPackageName cases above.
		{"crypto/internal/fips140/bigmod.table", true},
		{"crypto/internal/fips140/v1.26.0/bigmod.table", true},
		{"golang.org/x/tools/internal.Version", true},
		{"__imp_example.com/m/pkg.Global", true},

		// Ordinary containers the scan must look inside.
		{"main.StaticErr", false},
		{"example.com/m/pkg.Table", false},
		{"main..dict.boxAny[main.T]", false},
		{"net/http.statusText", false},
		{"mypkg/notinternal/x.Table", false},
		{"github.com/internal-tools/cli.Flags", false},

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

// TestInternalPkgScopingShared pins the thing trendvidia/go-archive#111 is
// actually about: the internal-package scoping rule has ONE definition,
// and the two gates that consume it cannot drift apart again.
//
// They had drifted. Both spelled the rule strings.HasPrefix(name,
// "internal/"), which is not Go's rule — "internal" is a path ELEMENT —
// so every nested internal package was admitted as user code by both.
// A copy that is wrong twice looks consistent, which is why the fix is a
// shared definition rather than two corrections.
func TestInternalPkgScopingShared(t *testing.T) {
	internal := []string{
		"internal/abi.Kind",
		"internal/bytealg.MaxLen",
		"crypto/internal/fips140/bigmod.table",
		"crypto/internal/fips140/v1.26.0/bigmod.table",
		"crypto/internal/boring.ErrOpen",
		"golang.org/x/tools/internal/event.Err",
		"golang.org/x/tools/internal.Version",
	}
	notInternal := []string{
		"mypkg/notinternal/x.Y",
		"github.com/internal-tools/cli.Verbose",
		"example.com/m/internalish.Global",
		"example.com/m/pkg.InternalCounter",
		"example.com/m/pkg.Table",
		"os.Args",
	}
	for _, name := range internal {
		if !isInternalPkgName(name) {
			t.Errorf("isInternalPkgName(%q) = false, want true", name)
		}
		// The two consumers must agree, in their own directions:
		// nothing internal is registered for canonicalisation, and
		// nothing internal is scanned as a container.
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: an internal package must stay module-local", name)
		}
		if !skipStaticCanonContainer(name) {
			t.Errorf("skipStaticCanonContainer(%q) = false, want true: the gates disagree, which is the drift #111 removed", name)
		}
	}
	for _, name := range notInternal {
		if isInternalPkgName(name) {
			t.Errorf("isInternalPkgName(%q) = true, want false: \"internal\" must be a whole path element", name)
		}
	}
}

// TestUserDataExportable pins both halves of userDataLink's export
// gate: which NAMES may be registered for cross-module canonicalisation
// (isUserPackageName, covered above) and which symbol KINDS
// (trendvidia/go-archive#106).
//
// The kind half exists because of an asymmetry nothing else notices:
// staticdata.WriteFuncSyms emits a funcval as RODATA in an ordinary
// build and as NOPTR — writable, so Plugin.Close can overwrite the code
// pointer — under -dynlink. A plugin's funcvals therefore landed in the
// writable kinds this walk always covered, while a plugin HOST's landed
// in SRODATA and were skipped. The host published no funcvals at all,
// so neither consumer of canonicalSyms (canonicalizeSlots for the
// funcptr slots, userDataInit for data words) had anything to
// canonicalise a plugin's copy against: a func value materialised in a
// plugin for a host-shared function kept the plugin's own funcval,
// whose word is the plugin's IAT thunk.
//
// SRODATA is admitted for funcvals ONLY. The rest of that section is
// type descriptors, strings and the slot carriers — per-module by
// design, and mostly rejected by name already; the kind filter is what
// stops a future rename from letting one through.
func TestUserDataExportable(t *testing.T) {
	const funcval = "testplugin/funcident/shared.Fn·f"

	// A funcval is exportable in BOTH kinds it can be emitted as: the
	// plugin's writable copy and the host's read-only one. Registering
	// only one of them is the bug.
	for _, st := range []sym.SymKind{sym.SRODATA, sym.SNOPTRDATA} {
		if !userDataExportable(funcval, st) {
			t.Errorf("userDataExportable(%q, %v) = false, want true: a %v funcval is left out of the "+
				"registry, so nothing canonicalises a peer module's copy against it", funcval, st, st)
		}
	}

	// Ordinary package-level data, unchanged by #106.
	for _, st := range []sym.SymKind{sym.SDATA, sym.SBSS, sym.SNOPTRDATA, sym.SNOPTRBSS} {
		if !userDataExportable("testplugin/common.X", st) {
			t.Errorf("userDataExportable(testplugin/common.X, %v) = false, want true", st)
		}
	}

	// SRODATA is admitted for funcvals only: everything else in that
	// section is per-module.
	for _, name := range []string{
		"testplugin/common.X",
		"type:testplugin/common.T",
		"go:string.hello",
		"go.funcptr.testplugin/common.F·f",
		"runtime.funcptrlinks",
	} {
		if userDataExportable(name, sym.SRODATA) {
			t.Errorf("userDataExportable(%q, SRODATA) = true, want false: only funcvals are read-only "+
				"symbols eligible for cross-module canonicalisation", name)
		}
	}

	// Kinds that are never eligible, funcval name or not.
	for _, st := range []sym.SymKind{sym.STEXT, sym.SDYNIMPORT, sym.SHOSTOBJ, sym.SWINDOWS} {
		if userDataExportable(funcval, st) {
			t.Errorf("userDataExportable(%q, %v) = true, want false", funcval, st)
		}
	}

	// A STDLIB funcval is exportable too, since trendvidia/go-archive#115
	// dropped the slashless arm. That closes the residual #106 left
	// open: host and plugin now agree on the identity of a stdlib
	// function value, not just a user-package one.
	for _, st := range []sym.SymKind{sym.SRODATA, sym.SNOPTRDATA} {
		if !userDataExportable("os.Exit·f", st) {
			t.Errorf("userDataExportable(os.Exit·f, %v) = false, want true: host and plugin would "+
				"disagree on the identity of a stdlib function value", st)
		}
	}

	// The name gate still applies to funcvals: a funcval owned by an
	// excluded subsystem must not be registered just because its kind
	// qualifies.
	for _, name := range []string{
		"runtime.gcenable·f",
		"internal/poll.CloseFunc·f",
		"testplugin/common..stmp_1·f",
	} {
		for _, st := range []sym.SymKind{sym.SRODATA, sym.SNOPTRDATA} {
			if userDataExportable(name, st) {
				t.Errorf("userDataExportable(%q, %v) = true, want false: the name gate applies to funcvals too", name, st)
			}
		}
	}
}

// TestIsFuncValName pins the funcval spelling this package depends on.
// It is ir.FuncSymName's output — the function's symbol name plus a
// MIDDLE DOT (U+00B7) and 'f' — not an ASCII dot, and not a suffix any
// user-declared identifier can produce, since U+00B7 is not legal in a
// Go identifier.
func TestIsFuncValName(t *testing.T) {
	for _, name := range []string{
		"testplugin/common.F·f",
		"i106/shared.Fn·f",
		"testplugin/common.(*T).M·f",
		"testplugin/common.F.func1·f",
	} {
		if !isFuncValName(name) {
			t.Errorf("isFuncValName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"testplugin/common.F",
		"testplugin/common.Ff",
		"testplugin/common.F.f", // ASCII dot, not the middle dot
		"go.funcptr.testplugin/common.F·f.slot",
		"",
	} {
		if isFuncValName(name) {
			t.Errorf("isFuncValName(%q) = true, want false", name)
		}
	}
}
