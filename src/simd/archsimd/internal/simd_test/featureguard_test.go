// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"internal/testenv"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestFeatureGuards pins that every archsimd operation this package's tests
// and benchmarks reach is reached only under a runtime check for the CPU
// feature the operation needs.
//
// What an operation needs comes from its doc comment ("Asm: ..., CPU
// Feature: X" or "Emulated, CPU Feature: X"); an undocumented wrapper such as
// Int32x8.ToMask needs whatever the archsimd operations it calls need. Vector
// width is not the predicate: a 128-bit type is SSE-class, but an emulated
// method on it may be built from an AVX2-only instruction (Uint8x16.Less goes
// through VPBROADCASTB), and calling it unguarded turns the package test into
// a SIGILL on any pre-Haswell amd64 CPU.
//
// TestMain gates on AVX, so that is the baseline; anything above it must sit
// under an `if archsimd.X86.F() { ... }` block or after an
// `if !archsimd.X86.F() { t.Skip(...) }` preamble. The check is static, so it
// fails on every amd64 host, not only on the ones that would crash.
//
// Not everything archsimd declares can be classified: a bodyless intrinsic
// with no CPU feature annotation (every raw Load/Store, and the Mask And/Or
// and ToInt conversions) has no requirement to read anywhere in the source.
// Those are counted and reported separately rather than silently treated as
// needing nothing, and unresolvedFamilies pins which kinds exist, so a new
// kind of unannotated declaration fails here instead of quietly widening the
// hole. Issue trendvidia/go-archive#97 tracks giving those declarations a feature.
func TestFeatureGuards(t *testing.T) {
	// The importer runs `go list -export` to find export data.
	testenv.MustHaveGoBuild(t)
	// No testing.Short() skip on purpose: cmd/dist runs every package test
	// with -short, so a short-skipped pin would guard nothing in all.bash.

	fset := token.NewFileSet()
	ctx := build.Default
	ctx.GOARCH = "amd64"
	ctx.BuildTags = append(ctx.BuildTags, "goexperiment.simd")

	arch := typeCheckDir(t, fset, &ctx, filepath.Join("..", ".."), "simd/archsimd", false, nil)
	graph := featureImplications(t, arch)
	need := requiredFeatures(arch, graph)
	checkUnresolved(t, need)
	declared := len(need)
	self := typeCheckDir(t, fset, &ctx, ".", "simd/archsimd/internal/simd_test", true, arch.pkg)

	checkFeatureNames(t, graph, need)
	r := newResolver(fset, graph, need, self)
	res := r.walk(self)
	if res.checked == 0 {
		t.Fatal("no archsimd references found; the checker is looking in the wrong place")
	}
	for _, v := range res.violations {
		t.Error(v)
	}
	t.Logf("checked %d references (%d more unresolved) against %d archsimd declarations",
		res.checked, res.unresolved, declared)
}

// entryPoint reports whether a function in the test package is an entry point
// the testing package calls directly. Everything else in the package is a
// helper, checked at its call sites.
func entryPoint(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, prefix) && name != "TestMain" {
			return true
		}
	}
	return false
}

type walkResult struct {
	checked    int
	unresolved int
	violations []string
}

// walk checks every entry point in pkg.
func (r *resolver) walk(pkg *checkedPkg) walkResult {
	var res walkResult
	for _, f := range pkg.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil || !entryPoint(fd.Name.Name) {
				continue
			}
			c := r.check(fd.Body, nil)
			res.violations = append(res.violations, c.violations...)
			res.checked += c.checked
			res.unresolved += c.unresolved
		}
	}
	sort.Strings(res.violations)
	return res
}

type checkedPkg struct {
	pkg   *types.Package
	files []*ast.File
	info  *types.Info
}

// typeCheckDir parses and type-checks the (external test, if xtest) package
// in dir as it would be built for GOARCH=amd64 with the simd experiment on.
// Dependencies come from the toolchain's export data, except known, which is
// used as-is.
func typeCheckDir(t *testing.T, fset *token.FileSet, ctx *build.Context, dir, path string, xtest bool, known *types.Package) *checkedPkg {
	t.Helper()
	bp, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	names := bp.GoFiles
	if xtest {
		names = bp.XTestGoFiles
	}
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return typeCheckFiles(t, fset, path, files, known)
}

func typeCheckFiles(t *testing.T, fset *token.FileSet, path string, files []*ast.File, known *types.Package) *checkedPkg {
	t.Helper()
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Instances:  map[*ast.Ident]types.Instance{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{
		Importer: &knownImporter{deflt: importer.ForCompiler(fset, "gc", nil), known: known},
		Sizes:    types.SizesFor("gc", "amd64"),
		Error:    func(err error) { t.Errorf("type-checking %s: %v", path, err) },
	}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		t.Fatalf("type-checking %s: %v", path, err)
	}
	return &checkedPkg{pkg: pkg, files: files, info: info}
}

type knownImporter struct {
	deflt types.Importer
	known *types.Package
}

func (i *knownImporter) Import(path string) (*types.Package, error) {
	if i.known != nil && path == i.known.Path() {
		return i.known, nil
	}
	return i.deflt.Import(path)
}

// featureGraph maps a CPU feature to every feature it transitively implies.
type featureGraph map[string]map[string]bool

// implies reports whether a true archsimd.X86.guard() check establishes feat.
func (g featureGraph) implies(guard, feat string) bool {
	return guard == feat || g[guard][feat]
}

// known reports whether feat is a feature archsimd's X86Features documents.
func (g featureGraph) known(feat string) bool {
	_, ok := g[feat]
	return ok
}

var (
	alsoRE   = regexp.MustCompile(`also supports ([A-Za-z0-9, ]+)\.`)
	bundleRE = regexp.MustCompile(`supports the ([A-Za-z0-9]+(?:\+[A-Za-z0-9]+)+) features`)
)

// featureImplications reads the implication graph out of archsimd's own
// X86Features documentation rather than restating it here: each accessor's
// doc says "If it returns true, then the CPU also supports A, B, and C.", and
// an accessor for a bundle says "supports the AVX512F+CD+BW+DQ+VL features".
// A hand-written table drifts from cpu.go silently and in both directions --
// AVX512VPCLMULQDQ deliberately carries no "also supports" line, while
// AVXVNNI, FMA, VAES and AVXAES do -- and getting it wrong in the unsound
// direction means the pin blesses a guard that does not establish the
// feature.
func featureImplications(t *testing.T, arch *checkedPkg) featureGraph {
	t.Helper()
	direct := map[string][]string{}
	for _, f := range arch.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Doc == nil {
				continue
			}
			if !isFeatureAccessor(fd) {
				continue
			}
			name := fd.Name.Name
			direct[name] = append(direct[name], bundleMembers(t, name, fd.Doc)...)
			direct[name] = append(direct[name], alsoSupports(fd.Doc)...)
		}
	}
	if len(direct) == 0 {
		t.Fatal("no X86Features accessors found in archsimd; cpu.go changed shape")
	}
	// The bundle accessor is the one assumption worth failing on: if AVX512
	// stops covering AVX512F/BW/CD/DQ/VL, every annotation naming a member
	// silently stops being covered by an AVX512 guard.
	for _, member := range []string{"AVX512F", "AVX512CD", "AVX512BW", "AVX512DQ", "AVX512VL"} {
		if !contains(direct["AVX512"], member) {
			t.Fatalf("archsimd X86Features.AVX512 no longer documents %s as part of the bundle; got %v",
				member, direct["AVX512"])
		}
	}

	nodes := map[string]bool{}
	for name, imp := range direct {
		nodes[name] = true
		for _, f := range imp {
			nodes[f] = true
		}
	}
	g := featureGraph{}
	for name := range nodes {
		closure := map[string]bool{}
		var visit func(string)
		visit = func(f string) {
			for _, next := range direct[f] {
				if !closure[next] {
					closure[next] = true
					visit(next)
				}
			}
		}
		visit(name)
		delete(closure, name)
		g[name] = closure
	}
	return g
}

// bundleMembers expands a bundle accessor's "supports the X+A+B features"
// doc line into the full feature names.
func bundleMembers(t *testing.T, name string, doc *ast.CommentGroup) []string {
	t.Helper()
	m := bundleRE.FindStringSubmatch(doc.Text())
	if m == nil {
		return nil
	}
	parts := strings.Split(m[1], "+")
	if !strings.HasPrefix(parts[0], name) {
		t.Fatalf("archsimd X86Features.%s documents bundle %q, which does not extend %s", name, m[1], name)
	}
	out := []string{parts[0]}
	for _, p := range parts[1:] {
		out = append(out, name+p)
	}
	return out
}

// alsoSupports returns the features an accessor's doc says are implied.
func alsoSupports(doc *ast.CommentGroup) []string {
	m := alsoRE.FindStringSubmatch(doc.Text())
	if m == nil {
		return nil
	}
	list := strings.ReplaceAll(m[1], ", and ", ", ")
	list = strings.ReplaceAll(list, " and ", ", ")
	var out []string
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

var featureRE = regexp.MustCompile(`CPU Features?:\s*([A-Za-z0-9_]+)`)

// requirement is what one declaration needs. An empty feats with unknown
// false means "needs nothing above the baseline"; unknown means at least one
// declaration reachable from here carries no requirement anywhere in the
// source, so feats is a lower bound rather than the answer.
type requirement struct {
	feats   []string
	unknown bool
	// opaque marks the declaration itself as the source of the unknown: no
	// body to walk and no annotation to read. Callers of one inherit unknown
	// but are not opaque themselves.
	opaque bool
}

// requiredFeatures returns, for every function and method archsimd declares,
// the CPU features it needs: the documented one, or for an undocumented
// wrapper the union of what the archsimd functions it calls need. A bodyless
// declaration with no annotation is unknown -- there is nothing left to read
// -- and unknown propagates to its callers, so a wrapper over one is not
// mistaken for a wrapper over nothing.
func requiredFeatures(arch *checkedPkg, graph featureGraph) map[types.Object]requirement {
	declared := map[types.Object][]string{}
	callees := map[types.Object][]types.Object{}
	bodyless := map[types.Object]bool{}
	all := map[types.Object]bool{}
	for _, f := range arch.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			obj := arch.info.Defs[fd.Name]
			if obj == nil || isFeatureAccessor(fd) {
				// The X86Features accessors are the guards themselves, not
				// SIMD operations; leaving them out keeps the reference
				// count a count of operations.
				continue
			}
			all[obj] = true
			if fd.Doc != nil {
				for _, c := range fd.Doc.List {
					if m := featureRE.FindStringSubmatch(c.Text); m != nil {
						declared[obj] = []string{m[1]}
					}
				}
			}
			if fd.Body == nil {
				bodyless[obj] = true
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if fn, ok := arch.info.Uses[id].(*types.Func); ok && fn.Pkg() == arch.pkg {
						callees[obj] = append(callees[obj], origin(fn))
					}
				}
				return true
			})
		}
	}
	need := map[types.Object]requirement{}
	var resolve func(obj types.Object, path map[types.Object]bool) requirement
	resolve = func(obj types.Object, path map[types.Object]bool) requirement {
		if f, ok := declared[obj]; ok {
			return requirement{feats: f}
		}
		if r, ok := need[obj]; ok {
			return r
		}
		if path[obj] {
			return requirement{}
		}
		if bodyless[obj] {
			need[obj] = requirement{unknown: true, opaque: true}
			return need[obj]
		}
		path[obj] = true
		var r requirement
		for _, c := range callees[obj] {
			cr := resolve(c, path)
			r.feats = append(r.feats, cr.feats...)
			r.unknown = r.unknown || cr.unknown
		}
		delete(path, obj)
		r.feats = graph.reduce(r.feats)
		need[obj] = r
		return r
	}
	for obj := range all {
		if f, ok := declared[obj]; ok {
			need[obj] = requirement{feats: f}
		}
	}
	for obj := range all {
		resolve(obj, map[types.Object]bool{})
	}
	return need
}

// isFeatureAccessor reports whether fd is a method on archsimd's X86Features.
func isFeatureAccessor(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	id, ok := fd.Recv.List[0].Type.(*ast.Ident)
	return ok && id.Name == "X86Features"
}

// unresolvedFamilies names the kinds of archsimd declaration that carry no
// CPU feature anywhere in the source, with why each is unclassifiable. It is
// a pin, not a permission: a declaration outside these families means
// archsimd grew a new unannotated intrinsic, and the check below says so.
// Resolving them for real is issue trendvidia/go-archive#97.
var unresolvedFamilies = []struct {
	name  string
	match func(fn *types.Func) bool
}{
	// Reinterprets between vector types of the same width: no instruction.
	{"As* reinterpret", func(fn *types.Func) bool { return strings.HasPrefix(fn.Name(), "As") }},
	// The same reinterprets under the names go1.27 gave them. As* is now a
	// one-line wrapper (ops_amd64.go: AsUint16x8 calls ReshapeToUint16s), so
	// the bodyless intrinsic moved to these four spellings: ReshapeTo<T>s
	// between element widths, BitsTo<T> and ToBits across the float/integer
	// boundary, and ConvertTo<T> between signed and unsigned of one width.
	// All are pure reinterpretation, so none names an instruction. Real
	// conversions (Float32x4.ConvertToInt32 and friends) are annotated and
	// never reach here.
	{"go1.27 reinterpret primitive", func(fn *types.Func) bool {
		for _, p := range []string{"ReshapeTo", "BitsTo", "ConvertTo"} {
			if strings.HasPrefix(fn.Name(), p) {
				return true
			}
		}
		return fn.Name() == "ToBits"
	}},
	// Raw loads and stores, e.g. LoadInt8x64 and Int8x64.Store. VMOVDQU on
	// an X, Y or Z register: the width decides the feature, which the source
	// does not state. go1.27 added the array-typed forms LoadInt8x64Array
	// (matched by the Load prefix) and Int8x64.StoreArray.
	{"raw load/store", func(fn *types.Func) bool {
		return strings.HasPrefix(fn.Name(), "Load") ||
			fn.Name() == "Store" || fn.Name() == "StoreArray"
	}},
	// Mask operations and the mask/vector conversions: represented as k
	// registers on AVX512 and as vectors below it, again unstated.
	{"mask op", func(fn *types.Func) bool {
		if fn.Name() == "asMask" {
			return true
		}
		recv := fn.Type().(*types.Signature).Recv()
		if recv == nil || !strings.HasPrefix(types.TypeString(recv.Type(), nil), "simd/archsimd.Mask") {
			return false
		}
		return fn.Name() == "And" || fn.Name() == "Or" || strings.HasPrefix(fn.Name(), "ToInt")
	}},
}

// checkUnresolved fails if archsimd declares an unannotated intrinsic outside
// the families listed above.
func checkUnresolved(t *testing.T, need map[types.Object]requirement) {
	t.Helper()
	var unexpected []string
	for obj, r := range need {
		if !r.opaque {
			continue
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			continue
		}
		known := false
		for _, fam := range unresolvedFamilies {
			if fam.match(fn) {
				known = true
				break
			}
		}
		if !known {
			unexpected = append(unexpected, fn.FullName())
		}
	}
	sort.Strings(unexpected)
	for _, name := range unexpected {
		t.Errorf("%s has no CPU feature annotation and no body: the guard pin cannot check its callers", name)
	}
}

// checkFeatureNames fails if archsimd annotates an operation with a feature
// X86Features has no accessor for: no guard could then establish it, so the
// pin would demand something no test can write.
func checkFeatureNames(t *testing.T, graph featureGraph, need map[types.Object]requirement) {
	t.Helper()
	seen := map[string]bool{}
	for obj, r := range need {
		for _, feat := range r.feats {
			if graph.known(feat) || seen[feat] {
				continue
			}
			seen[feat] = true
			t.Errorf("%s is annotated CPU Feature: %s, which archsimd.X86 cannot report", obj, feat)
		}
	}
}

// reduce drops duplicates and features implied by another in the set.
func (g featureGraph) reduce(feats []string) []string {
	var uniq []string
	seen := map[string]bool{}
	for _, f := range feats {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	var out []string
	for i, f := range uniq {
		redundant := false
		for j, other := range uniq {
			if i == j || !g.implies(other, f) {
				continue
			}
			// Two spellings of the same feature (AVX512 and AVX512F) imply
			// each other; keep the first so the set does not empty out.
			if g.implies(f, other) && j > i {
				continue
			}
			redundant = true
			break
		}
		if !redundant {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// resolver runs guard checks over one test package's functions. A helper
// (any non-entry-point function) is not held to the AVX baseline itself: what
// it needs is whatever its own unguarded references need, and that
// requirement is checked at each call site in an entry point, the same way an
// undocumented archsimd wrapper inherits what it calls.
type resolver struct {
	fset    *token.FileSet
	graph   featureGraph
	info    *types.Info
	need    map[types.Object]requirement
	helpers map[types.Object]*ast.FuncDecl
	busy    map[types.Object]bool
	// instantiated generic helpers, keyed by declaration and type arguments
	instNeed map[string]requirement
	instBusy map[string]bool
	pkg      *types.Package
}

func newResolver(fset *token.FileSet, graph featureGraph, need map[types.Object]requirement, pkg *checkedPkg) *resolver {
	helpers := map[types.Object]*ast.FuncDecl{}
	for _, f := range pkg.files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && !entryPoint(fd.Name.Name) {
				helpers[pkg.info.Defs[fd.Name]] = fd
			}
		}
	}
	// The helper requirements this resolver computes must not leak into the
	// archsimd declaration set: the reported counts, and any later run,
	// describe archsimd.
	own := make(map[types.Object]requirement, len(need))
	for k, v := range need {
		own[k] = v
	}
	return &resolver{fset: fset, graph: graph, info: pkg.info, need: own, helpers: helpers,
		instNeed: map[string]requirement{}, instBusy: map[string]bool{}, pkg: pkg.pkg}
}

func (r *resolver) check(body *ast.BlockStmt, subst map[*types.TypeParam]types.Type) *guardChecker {
	c := &guardChecker{r: r, guards: []string{"AVX"}, subst: subst}
	c.block(body)
	return c
}

// require returns the features fn needs, computing a helper's on first use.
func (r *resolver) require(fn types.Object) (requirement, bool) {
	if req, ok := r.need[fn]; ok {
		return req, true
	}
	fd, ok := r.helpers[fn]
	if !ok {
		return requirement{}, false
	}
	if r.busy == nil {
		r.busy = map[types.Object]bool{}
	}
	if r.busy[fn] {
		return requirement{}, false
	}
	r.busy[fn] = true
	c := r.check(fd.Body, nil)
	delete(r.busy, fn)
	r.need[fn] = requirement{feats: r.graph.reduce(c.uncovered), unknown: c.unresolved > 0}
	return r.need[fn], true
}

// requireInst returns the features one instantiation of a generic helper
// needs. A generic helper's body says nothing by itself: `v.StorePart(s)` on
// a type parameter resolves to the constraint's interface method, which is
// no archsimd object, so the plain require would report the helper as
// needing nothing however it is instantiated. That is how
// TestStorePartReturnValues128 reached Int32x4.StorePart (VPMASKMOVD, AVX2)
// unguarded with the pin green, trendvidia/go-archive#156. Walking the body once per
// instantiation, with the type arguments substituted for the parameters,
// lets the selector checker find the concrete method.
func (r *resolver) requireInst(fn types.Object, subst map[*types.TypeParam]types.Type) (requirement, bool) {
	fd, ok := r.helpers[fn]
	if !ok {
		return requirement{}, false
	}
	sig := fn.Type().(*types.Signature)
	key := fn.Name()
	for i := 0; i < sig.TypeParams().Len(); i++ {
		key += "|" + types.TypeString(subst[sig.TypeParams().At(i)], nil)
	}
	if req, ok := r.instNeed[key]; ok {
		return req, true
	}
	if r.instBusy[key] {
		return requirement{}, false
	}
	r.instBusy[key] = true
	c := r.check(fd.Body, subst)
	delete(r.instBusy, key)
	r.instNeed[key] = requirement{feats: r.graph.reduce(c.uncovered), unknown: c.unresolved > 0}
	return r.instNeed[key], true
}

type guardChecker struct {
	r      *resolver
	guards []string
	// subst maps the enclosing generic helper's type parameters to the
	// arguments of the instantiation being checked; nil outside generics.
	subst      map[*types.TypeParam]types.Type
	checked    int
	unresolved int
	uncovered  []string
	violations []string
}

// block walks a statement list, threading the guards established by
// `if !archsimd.X86.F() { t.Skip(...) }` preambles into the statements that
// follow them.
func (c *guardChecker) block(b *ast.BlockStmt) {
	saved := c.guards
	defer func() { c.guards = saved }()
	for _, s := range b.List {
		if ifs, ok := s.(*ast.IfStmt); ok {
			if neg := negatedFeatures(ifs.Cond); len(neg) > 0 && exits(ifs.Body) {
				c.stmt(ifs.Body)
				if ifs.Else != nil {
					c.stmt(ifs.Else)
				}
				c.guards = append(append([]string(nil), c.guards...), neg...)
				continue
			}
		}
		c.stmt(s)
	}
}

func (c *guardChecker) stmt(s ast.Node) {
	ast.Inspect(s, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if n.Init != nil {
				c.stmt(n.Init)
			}
			c.stmt(n.Cond)
			saved := c.guards
			c.guards = append(append([]string(nil), saved...), positiveFeatures(n.Cond)...)
			c.block(n.Body)
			c.guards = saved
			if n.Else != nil {
				c.stmt(n.Else)
			}
			return false
		case *ast.BlockStmt:
			c.block(n)
			return false
		case *ast.FuncLit:
			c.block(n.Body)
			return false
		case *ast.SelectorExpr:
			c.typeParamMethod(n)
		case *ast.Ident:
			c.ref(n)
		}
		return true
	})
}

// typeParamMethod checks a method call on a type-parameter receiver against
// the concrete method of the substituted type argument. Without it the call
// is a reference to the constraint's interface method, which ref ignores.
func (c *guardChecker) typeParamMethod(sel *ast.SelectorExpr) {
	s, ok := c.r.info.Selections[sel]
	if !ok || s.Kind() != types.MethodVal {
		return
	}
	recv := s.Recv()
	if p, ok := recv.(*types.Pointer); ok {
		recv = p.Elem()
	}
	tp, ok := recv.(*types.TypeParam)
	if !ok {
		return
	}
	concrete, ok := c.subst[tp]
	if !ok {
		return
	}
	obj, _, _ := types.LookupFieldOrMethod(concrete, true, c.r.pkg, sel.Sel.Name)
	if fn, ok := obj.(*types.Func); ok {
		c.refFunc(sel.Sel, fn, nil)
	}
}

// ref checks one reference to an archsimd function or method against the
// guards in force.
func (c *guardChecker) ref(id *ast.Ident) {
	fn, ok := c.r.info.Uses[id].(*types.Func)
	if !ok {
		return
	}
	// A call of a generic helper carries its type arguments here; map any
	// that are themselves parameters of the enclosing helper through subst.
	var subst map[*types.TypeParam]types.Type
	if inst, ok := c.r.info.Instances[id]; ok && inst.TypeArgs != nil && inst.TypeArgs.Len() > 0 {
		if sig, ok := origin(fn).Type().(*types.Signature); ok && sig.TypeParams().Len() == inst.TypeArgs.Len() {
			subst = map[*types.TypeParam]types.Type{}
			for i := 0; i < inst.TypeArgs.Len(); i++ {
				arg := inst.TypeArgs.At(i)
				if tp, ok := arg.(*types.TypeParam); ok {
					if outer, ok := c.subst[tp]; ok {
						arg = outer
					}
				}
				subst[sig.TypeParams().At(i)] = arg
			}
		}
	}
	c.refFunc(id, fn, subst)
}

// refFunc checks one reference to fn, made at id, against the guards in
// force; subst is set for a call of a generic helper.
func (c *guardChecker) refFunc(id *ast.Ident, fn *types.Func, subst map[*types.TypeParam]types.Type) {
	var req requirement
	var ok bool
	if subst != nil {
		req, ok = c.r.requireInst(origin(fn), subst)
	} else {
		req, ok = c.r.require(origin(fn))
	}
	if !ok {
		return
	}
	if req.unknown {
		c.unresolved++
	} else {
		c.checked++
	}
	for _, feat := range req.feats {
		covered := false
		for _, g := range c.guards {
			if c.r.graph.implies(g, feat) {
				covered = true
				break
			}
		}
		if !covered {
			c.uncovered = append(c.uncovered, feat)
			c.violations = append(c.violations, fmt.Sprintf("%s: %s needs %s but is reached under guard %v",
				c.r.fset.Position(id.Pos()), fn.FullName(), feat, c.guards))
		}
	}
}

// origin maps a generic instantiation back to the declaration it came from,
// which is the object the requirement is recorded against.
func origin(fn *types.Func) *types.Func {
	if o := fn.Origin(); o != nil {
		return o
	}
	return fn
}

// positiveFeatures returns the features a true cond establishes:
// archsimd.X86.F(), conjunctions of them, and the arch-qualified
// disjunction described at goarchTest.
func positiveFeatures(cond ast.Expr) []string {
	switch e := cond.(type) {
	case *ast.ParenExpr:
		return positiveFeatures(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.LAND {
			return append(positiveFeatures(e.X), positiveFeatures(e.Y)...)
		}
		if e.Op == token.LOR {
			// `runtime.GOARCH != "amd64" || archsimd.X86.F()`.
			if goarchTest(e.X, token.NEQ) {
				return positiveFeatures(e.Y)
			}
			if goarchTest(e.Y, token.NEQ) {
				return positiveFeatures(e.X)
			}
		}
	case *ast.CallExpr:
		if f := featureCheck(e); f != "" {
			return []string{f}
		}
	}
	return nil
}

// negatedFeatures returns the features a false cond establishes:
// !archsimd.X86.F(), disjunctions of them, and the arch-qualified
// conjunction described at goarchTest.
func negatedFeatures(cond ast.Expr) []string {
	switch e := cond.(type) {
	case *ast.ParenExpr:
		return negatedFeatures(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.LOR {
			return append(negatedFeatures(e.X), negatedFeatures(e.Y)...)
		}
		if e.Op == token.LAND {
			// `runtime.GOARCH == "amd64" && !archsimd.X86.F()`.
			if goarchTest(e.X, token.EQL) {
				return negatedFeatures(e.Y)
			}
			if goarchTest(e.Y, token.EQL) {
				return negatedFeatures(e.X)
			}
		}
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			return positiveFeatures(e.X)
		}
	}
	return nil
}

// goarchTest reports whether e is `runtime.GOARCH <op> "amd64"`.
//
// The multi-arch test files go1.27 introduced (compare_128_test.go and
// friends, built for amd64, arm64 and wasm) spell a guard that only
// applies on amd64 in one of two shapes:
//
//	if runtime.GOARCH != "amd64" || archsimd.X86.F() { ...op... }
//	if runtime.GOARCH == "amd64" && !archsimd.X86.F() { t.Skip(...) }
//
// Both admit the op exactly when the target is not amd64, or when F
// holds. Treating that as establishing F is sound here because every
// requirement in the map is read from an amd64 declaration -- this
// checker type-checks archsimd with GOARCH=amd64 -- so off amd64 there
// is no requirement to establish. Writing the plain guard instead would
// skip the op on arm64 and wasm, where no AVX feature is ever reported.
//
// Only these two shapes are accepted: a disjunction with any other left
// operand would not establish F on amd64 at all.
func goarchTest(e ast.Expr, op token.Token) bool {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op != op {
		return false
	}
	se, ok := be.X.(*ast.SelectorExpr)
	if !ok || se.Sel.Name != "GOARCH" {
		return false
	}
	if id, ok := se.X.(*ast.Ident); !ok || id.Name != "runtime" {
		return false
	}
	lit, ok := be.Y.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"amd64"`
}

// featureCheck returns F for a call archsimd.X86.F(), else "".
func featureCheck(call *ast.CallExpr) string {
	se, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := se.X.(*ast.SelectorExpr)
	if !ok || x.Sel.Name != "X86" {
		return ""
	}
	if id, ok := x.X.(*ast.Ident); !ok || id.Name != "archsimd" {
		return ""
	}
	return se.Sel.Name
}

// exits reports whether a block unconditionally leaves the enclosing test:
// t.Skip*, t.Fatal*, os.Exit or return as its last statement.
func exits(b *ast.BlockStmt) bool {
	if len(b.List) == 0 {
		return false
	}
	switch s := b.List[len(b.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		se, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch se.Sel.Name {
		case "Skip", "Skipf", "SkipNow", "Fatal", "Fatalf", "FailNow", "Exit":
			return true
		}
	}
	return false
}
