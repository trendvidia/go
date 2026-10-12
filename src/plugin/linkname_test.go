// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package plugin_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"internal/testenv"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// linknameDirective is one //go:linkname as the compiler records it.
// remote is empty for the one-argument form, where the compiler fills in
// pkgpath+"."+local itself (cmd/compile/internal/noder.(*noder).pragma).
type linknameDirective struct {
	local  string
	remote string
}

// parseStdPackage reads the std package whose source is in dir. It
// parses every non-test .go file regardless of build tags, because the
// linkname surface is a property of the source: plugin_windows.go has to
// be checked from a linux or darwin host too.
//
// It returns:
//
//   - every //go:linkname directive in the package, from ANYWHERE in a
//     file. The compiler resolves a directive by package-scope name
//     lookup (writer.go's pw.curpkg.Scope().Lookup(l.local)) and never
//     by the comment's position, which is why src/runtime/linkname.go
//     can be a file of bare directives attached to no declaration at
//     all. A scan that read only doc comments would not see those.
//
//   - defs, mapping each package-scope func, var and const name to
//     whether some declaration of it DEFINES the symbol -- a var, a
//     const, or a func with a body -- rather than merely declaring a
//     symbol defined elsewhere. Files of different GOOS variants are
//     merged, so a name defined in any of them counts as defined.
//
//   - the object files in the package, whose presence would break the
//     "bodyless func == defined elsewhere" reading of defs.
func parseStdPackage(t *testing.T, dir string) (directives []linknameDirective, defs map[string]bool, asm []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	defs = make(map[string]bool)
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		// .s is Go assembly, .S is assembly the C preprocessor runs
		// first (std has these under runtime/cgo), and .syso is a
		// pre-built object linked in wholesale. Any of the three can
		// define a symbol that a bodyless Go declaration then names, so
		// any of the three breaks the "bodyless == defined elsewhere"
		// reading below; matching only .s would let the other two
		// reclassify a push as a pull in silence.
		if strings.HasSuffix(name, ".s") || strings.HasSuffix(name, ".S") || strings.HasSuffix(name, ".syso") {
			asm = append(asm, name)
			continue
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				fields := strings.Fields(c.Text)
				if len(fields) < 2 || fields[0] != "//go:linkname" {
					continue
				}
				d := linknameDirective{local: fields[1]}
				if len(fields) > 2 {
					d.remote = fields[2]
				}
				directives = append(directives, d)
			}
		}
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					defs[decl.Name.Name] = defs[decl.Name.Name] || decl.Body != nil
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR && decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							defs[n.Name] = true
						}
					}
				}
			}
		}
	}
	return directives, defs, asm
}

// asmSymbolPattern matches a package-qualified symbol name in Go
// assembly, which spells the package separator as a middle dot. The
// leading boundary keeps "myplugin·x" from reading as "plugin·x".
func asmSymbolPattern(pkg string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(pkg) + `\x{00b7}`)
}

// asmMentions reports every line of the given assembly files under dir
// that names a symbol in pkg's namespace, as "file:NN: line".
//
// The //go:linkname scan cannot see these: an assembly file writes
// TEXT plugin·foo(SB) directly, with no directive to read. That is not
// hypothetical -- runtime defines runtime·addmoduledata in assembly in
// nine files, and it is one of the names
// cmd/link/internal/loader.blockedLinknames has to confine by hand. A
// definition landing in plugin's namespace this way would make the
// pinned set below incomplete while looking like nothing happened.
//
// It reports any mention rather than definitions only. Over-reporting is
// the safe direction for a pin: a reference to plugin·foo from runtime
// assembly is worth a human look too, and there are none today.
func asmMentions(t *testing.T, dir string, asm []string, pkg string) []string {
	t.Helper()
	re := asmSymbolPattern(pkg)
	var found []string
	for _, name := range asm {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if re.MatchString(code) {
				found = append(found, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
			}
		}
	}
	return found
}

// TestPluginPushLinknameSurface pins every symbol package plugin PUSHES
// into the global linkname namespace (trendvidia/go-archive#78).
//
// A //go:linkname whose local name resolves to a DEFINITION in this
// package -- a var, a const, or a func with a body -- is a PUSH, and per
// cmd/link/internal/loader.checkLinkname a push on the def is what makes
// the symbol legally pull-linknameable from any other package under the
// default -checklinkname=1. Both forms of the directive do this: the
// compiler sets sym.Linkname from either, and reader.go marks the def
// obj.AttrLinkname whenever that is non-empty and the symbol is local.
// The one-argument form is not the special case; DEFINING the symbol is.
//
// A push is not the ONLY way through checkLinkname, which allows
// osym.IsLinkname() || osym.ABIWrapper(). An ABI wrapper is reachable
// with no push at all, so the pinned set equals the reachable set only
// while package plugin has nothing wrapped. Wrappers attend assembly,
// and the asm guard below hard-fails the day this package gains any --
// that guard is what holds this equality, not an assumption.
//
// The same directive on a bodyless declaration is a PULL: the symbol is
// defined in the runtime, which chose to export it to package plugin,
// and nothing is exposed outward.
//
// #78 weighed one push and kept it: testHookBetweenFenceStages lets code
// linked into a host run inside open's TOCTOU window with pluginsMu
// held. It was accepted because anything linkable into the host is
// already inside the trust boundary and could call syscall.LoadLibrary
// directly, so the hook widens no boundary -- it makes an existing path
// look supported. The reasoning is recorded at the declaration in
// plugin_windows.go.
//
// That decision was about ONE symbol on a security-relevant path. It is
// not a general licence, and the risk is drift: a second push added
// later inherits the first one's justification without anyone weighing
// it. This test makes the set explicit so growing it is a decision
// someone makes rather than a detail that lands.
//
// If this fails because you added a push linkname: that is the design
// call #78 describes, not a test to update. Re-read that issue, weigh
// the new symbol on its own terms, and record the reasoning where the
// hook's is.
func TestPluginPushLinknameSurface(t *testing.T) {
	t.Parallel()
	testenv.MustHaveSource(t)

	dir := filepath.Join(testenv.GOROOT(t), "src", "plugin")
	directives, defs, asm := parseStdPackage(t, dir)

	if len(asm) > 0 {
		// A bodyless func backed by assembly in this package is a
		// definition, so a linkname on it is a push -- and defs cannot
		// see that from the Go source alone. Assembly is also what
		// brings ABI wrappers, checkLinkname's other allow-path, so
		// this one guard covers both ways the pinned set could stop
		// matching the reachable set. Package plugin has no object
		// files today; if it gains some, this classifier has to grow
		// before it can be trusted again.
		t.Fatalf("package plugin gained object files %v: a bodyless func may now be a definition, "+
			"so this test can no longer tell a push from a pull. Teach parseStdPackage to read the "+
			"assembly's TEXT symbols before removing this check.", asm)
	}

	var got, renamed []string
	for _, d := range directives {
		if !defs[d.local] {
			continue // a pull, or not a package-scope definition at all
		}
		if d.remote != "" && d.remote != "plugin."+d.local {
			// A definition renamed into somebody else's namespace. It
			// is reachable as d.remote, NOT as plugin.<local>, so it is
			// not part of the surface #78 weighed and does not belong
			// in the set below -- reporting it there would tell the
			// next reader to re-litigate #78 over a symbol that is not
			// in package plugin's namespace at all.
			//
			// It is still worth surfacing, because std writes
			// pull-a-variable exactly this way (reflect/value.go's
			// zeroVal, internal/runtime/maps/runtime.go's) and the Go
			// source cannot tell that apart from deliberately defining
			// another package's symbol.
			renamed = append(renamed, d.local+" -> "+d.remote)
			continue
		}
		got = append(got, d.local)
	}
	slices.Sort(got)
	got = slices.Compact(got)
	slices.Sort(renamed)
	renamed = slices.Compact(renamed)

	want := []string{"testHookBetweenFenceStages"}
	if !slices.Equal(got, want) {
		t.Errorf("package plugin's push-linkname surface changed.\n"+
			"  got:  %v\n"+
			"  want: %v\n\n"+
			"Each name here is defined in package plugin and reachable as plugin.<name> by "+
			"//go:linkname from any package linked into a host. trendvidia/go-archive#78 accepted "+
			"exactly one, on the reasoning recorded at testHookBetweenFenceStages in "+
			"plugin_windows.go; a new one does not inherit that reasoning. Weigh it and write "+
			"down the outcome.\n\n"+
			"If the name you added IS a bodyless declaration on the GOOS you build for, check "+
			"the other GOOS files: parseStdPackage merges them, so a stub with a body in "+
			"plugin_stubs.go makes the windows-only declaration of the same name read as a "+
			"definition. That over-reporting is deliberate -- a pin should fail loudly rather "+
			"than miss a push -- but it means the fix is to rename, not to weigh a new push.",
			got, want)
	}
	if len(renamed) > 0 {
		t.Errorf("package plugin now defines symbols in another package's namespace: %v\n\n"+
			"These are NOT part of the plugin.* surface trendvidia/go-archive#78 weighed, so do not "+
			"add them to the set above. Each one defines the symbol on the right, which is "+
			"either the std pull-a-variable idiom (harmless, see reflect/value.go's zeroVal) "+
			"or package plugin planting a definition in a package it does not own (not "+
			"harmless). Decide which this is; if it is the former, allow it here explicitly.",
			renamed)
	}
}

// TestRuntimePushesIntoPluginNamespace pins the other half of the
// plugin.* linkname surface, which #78 did not weigh.
//
// Package runtime pushes nine of its own definitions in under a
// plugin.* name. Each is reachable from any package for the same reason
// a push in package plugin is -- the def carries obj.AttrLinkname, so
// checkLinkname lets anyone reference it -- and eight were added by this
// fork. None is listed in cmd/link/internal/loader.blockedLinknames,
// which is where std confines every comparable runtime push to one
// package.
//
// This pins what src/runtime pushes, which is NOT the same as pinning
// everything that can land in plugin's namespace: any package may push a
// definition in under a plugin.* name, and only src/plugin and
// src/runtime are read here. Measured 2026-08-27, nothing else in src
// does -- the only other plugin.* directives in the tree are four PULLS
// in cmd/cgo/internal/testplugin/testdata/rejectchained/host_windows.go,
// fixture code linked into no std binary. A third pusher would go
// unnoticed, so widen this scan rather than trust the claim if that
// becomes plausible.
//
// trendvidia/go-archive#86 holds the open question of whether the nine should be
// confined. This test decides nothing; it records the set as it stands
// so a tenth cannot land unnoticed while that question is open.
func TestRuntimePushesIntoPluginNamespace(t *testing.T) {
	t.Parallel()
	testenv.MustHaveSource(t)

	dir := filepath.Join(testenv.GOROOT(t), "src", "runtime")
	directives, _, asm := parseStdPackage(t, dir)

	// Assembly defines symbols with no directive to read, so the scan
	// above is blind to a TEXT plugin·foo(SB) -- the same class that
	// forces runtime.addmoduledata, an assembly definition, into
	// blockedLinknames by hand. There are none today; this keeps it so.
	if mentions := asmMentions(t, dir, asm, "plugin"); len(mentions) > 0 {
		t.Errorf("runtime assembly now names symbols in package plugin's namespace:\n  %s\n\n"+
			"An assembly definition lands in that namespace without any //go:linkname, so the "+
			"directive scan below cannot see it and the pinned set is no longer the whole "+
			"surface. Weigh it under trendvidia/go-archive#86 with the nine directives, and teach this "+
			"test to carry it.", strings.Join(mentions, "\n  "))
	}

	// Deliberately no definition test here -- unlike the plugin case,
	// where the asm guard makes "bodyless == pull" safe to rely on.
	// Package runtime is full of assembly, so a bodyless func may well be
	// a definition, and a directive naming plugin.* is worth surfacing
	// either way: as a push it exposes runtime's implementation, and as a
	// pull it would be a dependency from runtime on package plugin, which
	// does not exist and would be at least as notable.
	var got []string
	for _, d := range directives {
		if strings.HasPrefix(d.remote, "plugin.") {
			got = append(got, d.remote)
		}
	}
	slices.Sort(got)
	got = slices.Compact(got)

	want := []string{
		"plugin.chainGrew",
		"plugin.chainTail",
		"plugin.lastmoduleinit",
		"plugin.pluginClose",
		"plugin.pluginDependents",
		"plugin.pluginHasDependents",
		"plugin.rejectChainedModule",
		"plugin.syncBeforeUnmap",
		"plugin.validatePkghashes",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the set of runtime symbols pushed into package plugin's namespace changed.\n"+
			"  got:  %v\n"+
			"  want: %v\n\n"+
			"Each name here is reachable by //go:linkname from any package linked into a host, "+
			"exactly as a push in package plugin is, and none is confined by "+
			"cmd/link/internal/loader.blockedLinknames. trendvidia/go-archive#86 is open on whether they "+
			"should be; adding to the set before it is answered widens a surface nobody has "+
			"weighed. Weigh the new one there and write down the outcome.", got, want)
	}
}
