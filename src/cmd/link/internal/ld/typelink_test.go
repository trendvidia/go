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
		"testplugin/common..stmp_1",
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true, want false: must stay module-local", name)
		}
	}
	// Known divergence, pinned rather than fixed (trendvidia/go#110):
	// the "no slash means stdlib" arm also rejects the root package of
	// a dotted module path. `module go.example.org` with its package at
	// the module root emits `go%2eexample%2eorg.Counter` — PathToPrefix
	// escapes the dots because the whole path is the last element — so
	// the name carries no slash and never reaches userDataExports, and
	// host and plugin keep separate instances on Windows. This is the
	// same class of hole as #108 but a different arm, and it predates
	// the `go.` fix (an unescaped `example.com.Counter` never existed).
	for _, name := range []string{
		"go%2eexample%2eorg.Counter",
		"example%2ecom.Counter",
	} {
		if isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = true: #110 fixed? update this pin and the comment above it", name)
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
	} {
		if isModuleSlotName(name) {
			t.Errorf("isModuleSlotName(%q) = true, want false", name)
		}
		if !isUserPackageName(name) {
			t.Errorf("isUserPackageName(%q) = false, want true: go.*-domain global must be registered for canonicalisation", name)
		}
		if peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = true, want false: go.*-domain global must be exportable for host-plugin sharing", name)
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
//     from a host, but isUserPackageName's "no slash" arm keeps them out
//     of the canonicalisation registry; on Windows the host's import
//     library is what shares them.
func TestGateAsymmetryOutsideSlotFamilies(t *testing.T) {
	for _, name := range []string{
		"testplugin/common..inittask",
		"sync..inittask",
		"os.Args",
		"strconv.isPrint16",
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
