// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"internal/testenv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestPEDefQuote(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		// Even a plain dotted name has to be quoted: binutils' .def
		// parser rejects it bare.
		{"runtime.mheap_", `"runtime.mheap_"`},
		{"runtime.(*mheap).alloc", `"runtime.(*mheap).alloc"`},
		{"$f32.4b189680", `"$f32.4b189680"`},
		// gclocals names are base64 and can end in '=' padding, which a
		// .def EXPORTS line would read as a rename.
		{"gclocals·ixmhidx+qfzqw/oEK9kn0A==", `"gclocals·ixmhidx+qfzqw/oEK9kn0A=="`},
		// Generic instantiations carry spaces (token separator) and
		// semicolons (comment introducer) inside the shape name.
		{"p.f[go.shape.struct { a int }]", `"p.f[go.shape.struct { a int }]"`},
		{"p.f[go.shape.struct { a int; b int }]", `"p.f[go.shape.struct { a int; b int }]"`},
	} {
		if got := peDefQuote(tc.name); got != tc.want {
			t.Errorf("peDefQuote(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPEHostExportRank(t *testing.T) {
	// The cross-module runtime ABI outranks everything else, so it is
	// what survives when a host has more symbols than a PE image can
	// export. Anything that ties with ordinary package symbols would be
	// a candidate for eviction.
	for _, name := range []string{
		"runtime.mheap_", "runtime.sched", "runtime.addmoduledata",
		"internal/sync.(*HashTrieMap[go.shape.string,go.shape.int]).Load",
		"runtime.findfunc",    // peSharedV1Funcs
		"testplugin/common.X", // peSharedRuntimeData
		// Init tasks: evicting one re-runs that package's init from the
		// plugin, which re-registers host-shared runtime hooks with
		// plugin-resident funcvals that dangle after Close
		// (trendvidia/go#61).
		"sync..inittask",
		"testplugin/common..inittask",
	} {
		if got := peHostExportRank(name); got != 0 {
			t.Errorf("peHostExportRank(%q) = %d, want 0 (cross-module ABI)", name, got)
		}
	}
	for _, name := range []string{"main.main", "fmt.Println", "os.Getenv"} {
		if got := peHostExportRank(name); got == 0 {
			t.Errorf("peHostExportRank(%q) = 0, want a lower rank than the runtime ABI", name)
		}
	}
}

// TestPEInittaskOutranksItsInits pins the ordering fact the runtime and
// the linker both reason from: in the export cut, a package's
// `..inittask` is never dropped while any of that package's init
// functions survives.
//
// Two places depend on it. skipHostSharedInits' unresolvable-PC backstop
// only fires for a task that stayed in the plugin's schedule (its
// `..inittask` was NOT exported) whose first init fn WAS exported and
// converted — this ordering is why that combination cannot arise.
// isMethodBodyName's documented `pkg.init.N` false positive is likewise
// harmless only while the task outranks the body.
func TestPEInittaskOutranksItsInits(t *testing.T) {
	for _, pkg := range []string{
		"sync",              // rank 1 inits, rank 0 task
		"internal/bytealg",  // both rank 0, decided by name
		"runtime",           // both rank 0, decided by name
		"p",                 // plain user package
		"gopkg.in/yaml.v2",  // package path containing dots
		"testplugin/common", // peSharedRuntimeData member
	} {
		task := pkg + "..inittask"
		for _, fn := range []string{pkg + ".init", pkg + ".init.0", pkg + ".init.1"} {
			if !peHostExportLess(task, fn) {
				t.Errorf("peHostExportLess(%q, %q) = false: the task can be evicted while its init body is exported, "+
					"which would leave the plugin a scheduled task calling into the host", task, fn)
			}
		}
	}
}

// TestPESkipSymName pins which double-dot symbol classes a plugin host
// publishes. Compiler-generated per-package `pkg..name` symbols are
// layout-specific and stay local — except `pkg..inittask`, which has to
// reach the host's exports and import library so peLoadImportLibrary can
// convert the plugin's copy and the plugin never re-runs an init the
// host already ran (trendvidia/go#61).
func TestPESkipSymName(t *testing.T) {
	for _, name := range []string{
		"sync..inittask",
		"internal/bytealg..inittask",
		"testplugin/common..inittask",
		"gopkg.in/yaml.v2..inittask",
	} {
		if peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = true, want false: host-sharing init tasks needs them exported", name)
		}
	}
	for _, name := range []string{
		"main..stmp_1",
		"main..interfaceSwitch.0",
		"p..typeAssert.0",
		"p..gcprog.a",
		"p..inittask.b", // interior, not the task itself
		"type..eq.p.T",
		// Slot names as the compiler actually spells them (measured
		// with `go tool nm` on a -gcflags=all=-typetab plugin build):
		// typeptr embeds the type symbol name, itabptr embeds the two
		// LinkStrings with no `go:itab.` segment in between.
		"go.typeptr.type:*p.T",
		"go.itabptr.*p.T,io.Reader",
		"go.funcptr.p.F",
		"_cgoexp_abcd1234_F",
		".rel",
	} {
		if !peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = false, want true: per-module symbol must stay local", name)
		}
	}
	// The slot-family filter must not swallow go.*-domain package
	// symbols: go.uber.org-style module paths share the `go.` prefix
	// with the slot families but their globals have to reach the host's
	// export table for host-plugin sharing (trendvidia/go#108).
	for _, name := range []string{
		"go.uber.org/zap.Global",
		"go.example.org/fakemod/lib.Counter",
	} {
		if peSkipSymName(name) {
			t.Errorf("peSkipSymName(%q) = true, want false: go.*-domain global must be exportable", name)
		}
	}
}

// TestPEHostExportsMatchImportLibrary builds a Windows plugin host and
// checks that its PE export table names exactly what the host.exe.a
// plugins link against names. A plugin imports what it finds in that
// archive's symbol index, so an entry the host does not export is one
// the OS loader cannot bind at plugin load time.
//
// The host is cross-built, so this runs on any platform. It covers the
// internal-link path, where the export table is the linker's own; the
// external-link path builds its .def from the same peHostExports set and
// is exercised end-to-end by cmd/cgo/internal/testplugin on Windows.
func TestPEHostExportsMatchImportLibrary(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	// Same fixture as the export-count tests, built once by one helper:
	// two copies of it meant two full windows/amd64 cross-builds per run
	// of this package, and two places for the host source to drift.
	exe := buildWindowsPE(t, peExportingMain)

	exports := peExportedNames(t, exe)
	if len(exports) == 0 {
		t.Fatal("host exports nothing; a plugin could not bind to it at all")
	}
	if len(exports) > peExportLimit {
		t.Errorf("host exports %d names, over the %d a PE image can address", len(exports), peExportLimit)
	}

	exported := make(map[string]bool, len(exports))
	for _, name := range exports {
		exported[name] = true
	}

	// Every name a plugin could resolve out of the import library has to
	// have a matching export. The BFD archive index also carries __imp_X
	// aliases and .idata helpers; peLoadImportLibrary maps the former
	// back to X and skips the latter, so do the same here.
	var missing []string
	for _, name := range peArchiveIndexNames(t, exe+".a") {
		name = strings.TrimPrefix(name, "__imp_")
		if peSkipSymName(name) || exported[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		if len(missing) > 10 {
			missing = append(missing[:10], fmt.Sprintf("... and %d more", len(missing)-10))
		}
		t.Errorf("import library names %d symbols the host does not export: %s",
			len(missing), strings.Join(missing, ", "))
	}

	// The cross-module runtime ABI has to be there: a plugin that cannot
	// bind these runs on its own allocator and scheduler.
	for _, name := range []string{
		"runtime.mheap_", "runtime.sched", "runtime.gcController",
		"runtime.addmoduledata", "runtime.lastmoduledatap", "runtime.writeBarrier",
		// Init tasks of host-linked packages: a plugin binding these gets
		// its copies converted and never re-runs an init the host already
		// ran (trendvidia/go#61). sync's is the load-bearing one — its
		// init registers runtime.poolcleanup, which is host-shared, and a
		// plugin-resident registration dangles after Close.
		"sync..inittask", "runtime..inittask",
	} {
		if !exported[name] {
			t.Errorf("host does not export %s, which plugins must share with it", name)
		}
	}

	// Per-module symbols must stay out. A plugin binding a section
	// boundary marker would point its own moduledata at the host's
	// sections and crash the GC; binding go:main.inittasks would run the
	// host's package initializers instead of its own.
	for name := range peSectionMarkers {
		if exported[name] {
			t.Errorf("host exports section boundary marker %s, which plugins must not bind", name)
		}
	}
	for _, name := range exports {
		if strings.HasPrefix(name, "go:") {
			t.Errorf("host exports linker-internal carrier %s, which is per-module", name)
			break
		}
	}
}

// TestPEPluginSharesHostInittasks cross-builds a Windows plugin host and
// a plugin against its import library, and checks the plugin binds the
// init tasks of host-linked packages as imports instead of keeping local
// copies. A local copy re-runs the package's init at plugin load; sync's
// init re-registers runtime.poolcleanup — a host-shared runtime global —
// with a funcval in the plugin's image, and the first GC after Close
// faults on it (trendvidia/go#61). Packages only the plugin links keep
// their tasks and still initialize.
func TestPEPluginSharesHostInittasks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module p61pin\n\ngo 1.26\n")
	write("host/main.go", `package main

import (
	"fmt"
	"plugin"
	"sync"
)

var pool = sync.Pool{New: func() any { return new(int) }}

func main() { fmt.Println(plugin.Open, pool.Get()) }
`)
	// leaf is linked only into the plugin, so its init has to keep
	// running plugin-side: its task must stay local.
	write("leaf/leaf.go", `package leaf

var State = "zero"

func init() { State = "initialized" }
`)
	write("plug/main.go", `package main

import (
	"sync"

	"p61pin/leaf"
)

var once sync.Once

func F() string { once.Do(func() {}); return leaf.State }

func main() {}
`)

	goBuild := func(args ...string) {
		t.Helper()
		cmd := testenv.Command(t, testenv.GoToolPath(t), append([]string{"build"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, out)
		}
	}
	goBuild("-o", filepath.Join("host", "host.exe"), "./host")
	goBuild("-buildmode=plugin",
		"-ldflags=-pluginimplib="+filepath.Join("host", "host.exe.a"),
		"-o", "plug.dll", "./plug")

	f, err := pe.Open(filepath.Join(dir, "plug.dll"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	imports, err := f.ImportedSymbols()
	if err != nil {
		t.Fatal(err)
	}
	imported := func(name string) bool {
		for _, imp := range imports {
			if imp == name || strings.HasPrefix(imp, name+":") {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"sync..inittask", "runtime..inittask"} {
		if !imported(name) {
			t.Errorf("plugin does not import %s: its local copy stays scheduled and the package's init re-runs at plugin load", name)
		}
	}
	if imported("p61pin/leaf..inittask") {
		t.Error("plugin imports p61pin/leaf..inittask, but the host does not link leaf: its init must stay plugin-local")
	}

	// Importing the task is only half of it: what actually stops the
	// duplicate init from running is the task being dropped from
	// `go:plugin.inittasks`, the list plugin_lastmoduleinit hands the
	// runtime. A regression that imported the task and still scheduled
	// it would satisfy every assertion above and re-run sync's init at
	// Open, which is the whole defect (trendvidia/go#61). Read the
	// schedule itself.
	scheduled := peScheduledInittasks(t, filepath.Join(dir, "plug.dll"))
	for _, name := range []string{"sync..inittask", "runtime..inittask"} {
		if scheduled[name] {
			t.Errorf("plugin schedules %s: the host already ran that init, and re-running it registers "+
				"plugin-resident funcvals into host-shared runtime hooks", name)
		}
	}
	// The positive half: a package only the plugin links has to keep
	// its task scheduled, or the plugin's own globals never initialize.
	if !scheduled["p61pin/leaf..inittask"] {
		t.Errorf("plugin does not schedule p61pin/leaf..inittask; scheduled tasks: %v", sortedKeys(scheduled))
	}

	// The plugin must not PUBLISH init tasks either. peSkipSymName lets
	// `pkg..inittask` through for the host's sake, and peExportAllSymbols
	// walks the same predicate, so a plugin would otherwise export the
	// tasks it kept — names nothing can bind (no plugin writes an import
	// library, and `pkg..inittask` is not a Go identifier so
	// plugin.Lookup cannot name one) that still spend the plugin's own
	// peExportLimit budget, which the link hard-errors past
	// (trendvidia/go#39).
	for _, name := range peExportedNames(t, filepath.Join(dir, "plug.dll")) {
		if strings.HasSuffix(name, "..inittask") {
			t.Errorf("plugin exports %s; a plugin's init tasks are unbindable and only consume its export budget", name)
		}
	}
}

// peExportedNames returns the names in a PE image's export directory.
// Delegates to peImageExportNames so the linker's own export-count check
// and every test that inspects an export table read the image the same
// way — the divergence #53 was about started as three copies of one walk.
func peExportedNames(t *testing.T, path string) []string {
	t.Helper()
	names, err := peImageExportNames(path)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// peArchiveIndexNames returns the names in an ar archive's "/" symbol
// index — the same index peLoadImportLibrary reads when a plugin links
// against the host.
func peArchiveIndexNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "!<arch>\n" {
		t.Fatalf("%s: not an ar archive", path)
	}
	var arHdr [60]byte
	if _, err := io.ReadFull(f, arHdr[:]); err != nil {
		t.Fatalf("%s: missing first member: %v", path, err)
	}
	if name := strings.TrimSpace(string(arHdr[0:16])); name != "/" {
		t.Fatalf("%s: first member is %q, want the %q symbol index", path, name, "/")
	}
	var size int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(arHdr[48:58])), "%d", &size); err != nil {
		t.Fatalf("%s: bad member size: %v", path, err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		t.Fatalf("%s: short symbol index: %v", path, err)
	}

	n := int(binary.BigEndian.Uint32(data[0:4]))
	names := data[4+n*4:]
	out := make([]string, 0, n)
	for i := 0; i < n && len(names) > 0; i++ {
		nul := bytes.IndexByte(names, 0)
		if nul < 0 {
			break
		}
		out = append(out, string(names[:nul]))
		names = names[nul+1:]
	}
	return out
}

// sortedKeys returns the keys of set in order, for stable failure output.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// peScheduledInittasks returns the names of the inittasks a plugin image
// actually schedules — the contents of `go:plugin.inittasks`, which
// plugin_lastmoduleinit passes to the runtime as md.inittasks.
//
// The COFF symbol table gives addresses but no sizes, so the list's
// extent is taken as the distance to the next symbol at a higher address
// (clamped to the section). That is exact here: inittaskSym emits the
// list as its own symbol, and a plugin with nothing left to initialize
// gets a zero-length one that coincides with the next symbol's address.
func peScheduledInittasks(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var imageBase uint64
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		imageBase = oh.ImageBase
	case *pe.OptionalHeader32:
		imageBase = uint64(oh.ImageBase)
	default:
		t.Fatalf("%s: no optional header", path)
	}

	type defined struct {
		name string
		va   uint64
		sec  *pe.Section
	}
	var defs []defined
	nameAt := make(map[uint64]string)
	for _, sym := range f.Symbols {
		if sym.SectionNumber <= 0 || int(sym.SectionNumber) > len(f.Sections) {
			continue // undefined (imported) or absolute
		}
		sec := f.Sections[sym.SectionNumber-1]
		va := imageBase + uint64(sec.VirtualAddress) + uint64(sym.Value)
		defs = append(defs, defined{sym.Name, va, sec})
		if _, dup := nameAt[va]; !dup {
			nameAt[va] = sym.Name
		}
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].va < defs[j].va })

	sched := -1
	for i, d := range defs {
		if d.name == "go:plugin.inittasks" {
			sched = i
			break
		}
	}
	if sched < 0 {
		t.Fatalf("%s: no go:plugin.inittasks symbol; the plugin has no init schedule to check", path)
	}
	start := defs[sched].va
	sec := defs[sched].sec
	end := imageBase + uint64(sec.VirtualAddress) + uint64(sec.VirtualSize)
	for i := sched + 1; i < len(defs); i++ {
		if defs[i].va > start {
			if defs[i].va < end {
				end = defs[i].va
			}
			break
		}
	}
	data, err := sec.Data()
	if err != nil {
		t.Fatal(err)
	}
	off := start - imageBase - uint64(sec.VirtualAddress)

	out := make(map[string]bool)
	for p := off; p+8 <= off+(end-start) && p+8 <= uint64(len(data)); p += 8 {
		ptr := binary.LittleEndian.Uint64(data[p : p+8])
		if ptr == 0 {
			// Alignment padding after the list, not an entry: the
			// schedule is a contiguous run of non-nil task pointers,
			// and the extent above is an upper bound.
			break
		}
		name, ok := nameAt[ptr]
		if !ok {
			t.Errorf("%s: go:plugin.inittasks[%d] = %#x, which is not a symbol defined in this image",
				path, (p-off)/8, ptr)
			continue
		}
		out[name] = true
	}
	return out
}

// TestPEImpWrapperTargetsIATSlot pins what a `__imp_`-named GOT target
// resolves to in an internal-linked Windows plugin (trendvidia/go#117).
//
// cmd/internal/obj/x86 rewrites the g-reload that follows an ABI0 call
// in a Windows plugin into a load from a symbol NAMED
// `__imp_runtime.tls_g` — deliberately, because an external link
// resolves that from the host's import library and so avoids the mingw
// pseudo-reloc-v2 overflow a direct memory ref would hit. peGotRedirect
// then wraps it, and the wrapper's CONTENT is what the sequence
// dereferences: it must be the address of the `.windynamic` IAT slot.
//
// Nothing defines `__imp_runtime.tls_g` under internal linking, so the
// wrapper used to hold the address of an undefined symbol — 0 in the
// image plus a base relocation, i.e. the rebasing DELTA at load. The
// g-reload then faulted inside plugin.Open, killing any plugin whose
// own text calls an ABI0 assembly function. A plugin importing
// crypto/ed25519 or net/http crashed on load; every fixture in the
// suite escaped it only because it shares its assembly-carrying
// packages with the host, so the call happens in host text, which is
// not -dynlink and uses the direct two-instruction form.
//
// The failure is silent at link time, which is why this reads the image
// rather than trusting a green build. hash/crc32 is the cheapest import
// that reaches the rewrite (measured: crc32 443KB vs crypto/sha256
// 908KB; hash/adler32 has no assembly and does not reach it at all),
// and the host below does not link it.
func TestPEImpWrapperTargetsIATSlot(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module p117pin\n\ngo 1.26\n")
	write("host/main.go", `package main

import (
	"fmt"
	"plugin"
)

func main() { fmt.Println(plugin.Open) }
`)
	write("plug/main.go", `package main

import "hash/crc32"

// Calls into crc32's ABI0 assembly, which the host does not link, so
// the g-reload after the call lands in PLUGIN text.
func F(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func main() {}
`)

	goBuild := func(args ...string) {
		t.Helper()
		cmd := testenv.Command(t, testenv.GoToolPath(t), append([]string{"build"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, out)
		}
	}
	goBuild("-o", filepath.Join("host", "host.exe"), "./host")
	goBuild("-buildmode=plugin",
		"-ldflags=-pluginimplib="+filepath.Join("host", "host.exe.a"),
		"-o", "plug.dll", "./plug")

	const wrapper = "__imp___imp_runtime.tls_g"
	word, ok := peSymbolWord(t, filepath.Join(dir, "plug.dll"), wrapper)
	if !ok {
		// Not a pass. If the compiler stops emitting the __imp_ form,
		// or crc32 stops carrying assembly, this test silently stops
		// exercising the bug it exists for.
		t.Fatalf("%s not present in plug.dll: this test no longer reaches the g-reload rewrite it pins", wrapper)
	}
	if word == 0 {
		t.Errorf("%s holds 0: the wrapper points at an undefined symbol, so at load it holds the "+
			"rebasing delta and the g-reload after an ABI0 call faults inside plugin.Open (trendvidia/go#117)", wrapper)
	}
}

// peSymbolWord returns the pointer-sized word stored at the address of
// the named symbol in a PE image, and whether that symbol is defined in
// it. Used to check what a synthesized slot actually holds, which is
// invisible to the symbol table alone.
func peSymbolWord(t *testing.T, path, name string) (uint64, bool) {
	t.Helper()
	f, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var imageBase uint64
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		imageBase = oh.ImageBase
	case *pe.OptionalHeader32:
		imageBase = uint64(oh.ImageBase)
	default:
		t.Fatalf("%s: no optional header", path)
	}

	for _, sym := range f.Symbols {
		if sym.Name != name || sym.SectionNumber <= 0 || int(sym.SectionNumber) > len(f.Sections) {
			continue
		}
		sec := f.Sections[sym.SectionNumber-1]
		data, err := sec.Data()
		if err != nil {
			t.Fatal(err)
		}
		off := uint64(sym.Value)
		if off+8 > uint64(len(data)) {
			// Past the raw data: a virtual-only (zero-filled) tail,
			// which reads as 0 at load.
			return 0, true
		}
		_ = imageBase
		return binary.LittleEndian.Uint64(data[off : off+8]), true
	}
	return 0, false
}

// TestPEInittaskSharesWithItsData pins the invariant peShareHostData
// exists to hold (trendvidia/go#74): a package's DATA is host-shared
// exactly when that package's `..inittask` is.
//
// The two disagreeing is not a cosmetic asymmetry. Sharing the task
// means the host already ran that init and the plugin drops it from its
// schedule, so whatever the init assigns is only ever assigned in the
// HOST's copy; leaving the data plugin-local hands plugin code an
// uninitialized global. `internal/poll.InitWSA` is a sync.OnceFunc
// value, so the plugin's copy is nil and `net.init.0` calls it —
// measured on windows/{amd64,arm64} as a null read inside plugin.Open.
// The internalinit fixture in cmd/cgo/internal/testplugin is the
// end-to-end half; this one needs no Windows host and no plugin build,
// which is what makes it the pin.
//
// A regression here is silent: it produces a plugin that links, loads,
// and only faults when something reaches one of those globals.
func TestPEInittaskSharesWithItsData(t *testing.T) {
	// Packages whose init assigns package-level state, spanning the
	// classes the gate used to treat differently: top-level internal/*,
	// nested .../internal/*, stdlib non-internal, and user code.
	for _, pkg := range []string{
		"internal/poll",
		"internal/bytealg",
		"internal/syscall/windows",
		"internal/godebug",
		"internal/runtime/gc/scan",
		"crypto/internal/fips140/bigmod",
		"sync",
		"os",
		"testplugin/common",
		"go.example.org/fakemod/lib",
		"example%2ecom",
	} {
		task := peShareHostData(pkg + "..inittask")
		data := peShareHostData(pkg + ".SomeGlobal")
		if task != data {
			t.Errorf("%s: inittask shared = %v but data shared = %v; the init runs in exactly one module, "+
				"so the other module's copy of what it assigns is never initialized (trendvidia/go#74)", pkg, task, data)
		}
		if !task {
			t.Errorf("%s..inittask is not host-shared: the plugin re-runs an init the host already ran", pkg)
		}
	}

	// runtime is the one deliberate exception, and stating it here is
	// what stops the loop above from being read as a blanket rule. Its
	// task must be shared — runtime's init must never re-run inside a
	// plugin — while its data is shared only through the curated
	// peSharedRuntimeData list, because some runtime singletons have to
	// stay plugin-local.
	if !peShareHostData("runtime..inittask") {
		t.Error("runtime..inittask is not host-shared: a plugin would re-run the runtime's init")
	}
	if peShareHostData("runtime.someUncuratedGlobal") {
		t.Error("an uncurated runtime global is host-shared; runtime data must go through peSharedRuntimeData")
	}
	for _, name := range []string{"runtime.writeBarrier", "runtime.tls_g", "runtime.lastmoduledatap"} {
		if !peShareHostData(name) {
			t.Errorf("%s is not host-shared: the plugin would keep its own copy of a process-wide singleton", name)
		}
	}

	// go: names are per-module carriers, never shared.
	for _, name := range []string{"go:link.pkghashes", "go:itab.*os.File,io.Reader", "go:buildinfo"} {
		if peShareHostData(name) {
			t.Errorf("%s is host-shared, but linker-internal carriers are per-module by design", name)
		}
	}
}

// TestIsMethodBodyName pins the receiver parse against the shape go1.27
// made reachable: a method whose NAME is package-qualified, because it
// was promoted from a type embedded from another package. Working back
// from the last dot reads the qualifying path as the receiver, and
// answering false there converts the ABIInternal body at v=1 and
// orphans the type descriptor's R_METHODOFF — the plugin link then
// fails with "missing section for relocation target ..."
// (trendvidia/go#139).
//
// Measured on a plugin host built from this tree: go1.26.8's import
// library carried 0 names of this shape and go1.27.1's carries 13.
func TestIsMethodBodyName(t *testing.T) {
	for _, name := range []string{
		// Package-qualified method names. These are the regression.
		"reflect.(*ptrType).internal/abi.descriptorSizes",
		"reflect.(*structType).internal/abi.descriptorSizes",
		"reflect.(*structTypeUncommon).internal/abi.descriptorSizes",
		"go.shape.(*struct { internal/sync.node = internal/sync.node[go.shape.interface {},go.shape.interface {}] }).internal/sync.entry",
		// Plain method bodies, which already worked.
		"runtime.(*mheap).alloc",
		"sync.(*Mutex).Lock",
		"gopkg.in/yaml.v2.(*decoder).unmarshal",
		"testplugin/common.T.Value",
	} {
		if !isMethodBodyName(name) {
			t.Errorf("isMethodBodyName(%q) = false, want true: converting this at v=1 orphans a type descriptor's R_METHODOFF", name)
		}
	}
	for _, name := range []string{
		// Package-level functions: one dot after the path.
		"fmt.Println",
		"internal/abi.Escape",
		"runtime.addmoduledata",
		// No dot at all.
		"main",
	} {
		if isMethodBodyName(name) {
			t.Errorf("isMethodBodyName(%q) = true, want false", name)
		}
	}

	// The value-receiver branch is deliberately conservative: a package
	// path whose last element contains a dot reads as `pkg.Recv.Method`
	// and answers true. isMethodBodyName's own comment accepts that
	// ("false positives at worst keep things plugin-local") and names
	// `pkg.init.N` as the measured case. Pinned rather than fixed, so
	// that tightening it is a deliberate change with this consequence
	// in view: each of these keeps its ABIInternal body in the plugin
	// instead of binding to the host's.
	for _, name := range []string{
		"gopkg.in/yaml.v2.Marshal",
		"sync.init.0",
	} {
		if !isMethodBodyName(name) {
			t.Errorf("isMethodBodyName(%q) = false; the documented conservative false positive changed. "+
				"That is not a bug by itself, but it changes which bodies bind to the host — re-read the "+
				"init-task reasoning in TestPEInittaskOutranksItsInits before accepting it.", name)
		}
	}
}
