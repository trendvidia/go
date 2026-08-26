// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package plugin_test

import (
	"bytes"
	"cmd/cgo/internal/cgotest"
	"context"
	"debug/elf"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"internal/platform"
	"internal/pluginabi"
	"internal/testenv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var globalSkip = func(t *testing.T) {}

var gcflags string = os.Getenv("GO_GCFLAGS")
var goroot string

// pluginSuffix is the file extension for plugin shared libraries.
var pluginSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func TestMain(m *testing.M) {
	flag.Parse()
	log.SetFlags(log.Lshortfile)
	os.Exit(testMain(m))
}

// tmpDir is used to cleanup logged commands -- s/tmpDir/$TMPDIR/
var tmpDir string

// prettyPrintf prints lines with tmpDir sanitized.
func prettyPrintf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if tmpDir != "" {
		s = strings.ReplaceAll(s, tmpDir, "$TMPDIR")
	}
	fmt.Print(s)
}

func testMain(m *testing.M) int {
	if testing.Short() && testenv.Builder() == "" {
		globalSkip = func(t *testing.T) { t.Skip("short mode and $GO_BUILDER_NAME not set") }
		return m.Run()
	}
	if !platform.BuildModeSupported(runtime.Compiler, "plugin", runtime.GOOS, runtime.GOARCH) {
		globalSkip = func(t *testing.T) { t.Skip("plugin build mode not supported") }
		return m.Run()
	}
	if !testenv.HasCGO() && runtime.GOOS != "windows" {
		globalSkip = func(t *testing.T) { t.Skip("cgo not supported") }
		return m.Run()
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	goroot = filepath.Join(cwd, "../../../../..")

	// Copy testdata into GOPATH/src/testplugin, along with a go.mod file
	// declaring the same path.

	GOPATH, err := os.MkdirTemp("", "plugin_test")
	if err != nil {
		log.Panic(err)
	}
	if os.Getenv("KEEP_TMPDIR") == "" {
		defer os.RemoveAll(GOPATH)
	}
	tmpDir = GOPATH
	fmt.Printf("TMPDIR=%s\n", tmpDir)

	modRoot := filepath.Join(GOPATH, "src", "testplugin")
	altRoot := filepath.Join(GOPATH, "alt", "src", "testplugin")
	for srcRoot, dstRoot := range map[string]string{
		"testdata":                           modRoot,
		filepath.Join("altpath", "testdata"): altRoot,
	} {
		if err := cgotest.OverlayDir(dstRoot, srcRoot); err != nil {
			log.Panic(err)
		}
		prettyPrintf("mkdir -p %s\n", dstRoot)
		prettyPrintf("rsync -a %s/ %s\n", srcRoot, dstRoot)

		if err := os.WriteFile(filepath.Join(dstRoot, "go.mod"), []byte("module testplugin\n"), 0666); err != nil {
			log.Panic(err)
		}
		prettyPrintf("echo 'module testplugin' > %s/go.mod\n", dstRoot)
	}

	os.Setenv("GOPATH", GOPATH)
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	} else {
		prettyPrintf("cd %s\n", modRoot)
	}
	os.Setenv("PWD", modRoot)

	os.Setenv("LD_LIBRARY_PATH", modRoot)

	if runtime.GOOS == "windows" {
		// On Windows, the host must be built first to generate the
		// import library that plugins link against.
		goCmd(nil, "build", "-o", "host.exe", "./host")
		hostImplib = filepath.Join(modRoot, "host.exe.a")
	}

	// Build plugin-mismatch from the alternate root.
	os.Setenv("GOPATH", filepath.Join(GOPATH, "alt"))
	if err := os.Chdir(altRoot); err != nil {
		log.Panic(err)
	} else {
		prettyPrintf("cd %s\n", altRoot)
	}
	os.Setenv("PWD", altRoot)
	goCmdBuildPlugin(nil, "-o", filepath.Join(modRoot, "plugin-mismatch"+pluginSuffix), "./plugin-mismatch")

	os.Setenv("GOPATH", GOPATH)
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	} else {
		prettyPrintf("cd %s\n", modRoot)
	}
	os.Setenv("PWD", modRoot)

	goCmdBuildPlugin(nil, "./plugin1")
	goCmdBuildPlugin(nil, "./plugin2")
	so, err := os.ReadFile("plugin2" + pluginSuffix)
	if err != nil {
		log.Panic(err)
	}
	if err := os.WriteFile("plugin2-dup"+pluginSuffix, so, 0444); err != nil {
		log.Panic(err)
	}
	prettyPrintf("cp plugin2%s plugin2-dup%s\n", pluginSuffix, pluginSuffix)

	goCmdBuildPlugin(nil, "-o=sub/plugin1"+pluginSuffix, "./sub/plugin1")
	goCmdBuildPlugin(nil, "-o=unnamed1"+pluginSuffix, "./unnamed1/main.go")
	goCmdBuildPlugin(nil, "-o=unnamed2"+pluginSuffix, "./unnamed2/main.go")

	if runtime.GOOS != "windows" {
		// On non-Windows, build host after plugins.
		goCmd(nil, "build", "-o", "host.exe", "./host")
	}

	// v2/L3 fixtures: each acts as its own plugin host. On Windows
	// each needs its own import library (each plugin's IAT names a
	// specific host by basename), so build the host first and use
	// goCmdBuildPluginFor to point its plugin at <host>.exe.a.
	goCmd(nil, "build", "-o", "closeapi.exe", "./closeapi")
	goCmdBuildPluginFor(nil, "closeapi", "-o=closeapi-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "panicstub.exe", "./panicstub")
	goCmdBuildPluginFor(nil, "panicstub", "-o=panicstub-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "lookuprace.exe", "./lookuprace")
	goCmdBuildPluginFor(nil, "lookuprace", "-o=lookuprace-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "closeunmaps.exe", "./closeunmaps")
	goCmdBuildPluginFor(nil, "closeunmaps", "-o=closeunmaps-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "refcount.exe", "./refcount/host")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugA"+pluginSuffix, "./refcount/plugA")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugB"+pluginSuffix, "./refcount/plugB")

	// Post-Close hazard-envelope fixture (trendvidia/go#26): one host
	// with subcommand-selected probes over an unprotected closure, an
	// exported-variable pointer, and the reopen-aliasing cell.
	goCmd(nil, "build", "-o", "hazards.exe", "./hazards")
	goCmdBuildPluginFor(nil, "hazards", "-o=hazards-plugin"+pluginSuffix, "./hazards/hazard")

	// Peer-plugin version-lock fixture (trendvidia/go#24). plugA and
	// plugC come from the main tree; plugB is built below from the
	// alternate root carrying a divergent build of the same
	// peerlock/peerlib import path that the host never links.
	goCmd(nil, "build", "-o", "peerlock.exe", "./peerlock")
	goCmdBuildPluginFor(nil, "peerlock", "-o=pm-plugA"+pluginSuffix, "./peerlock/plugA")
	goCmdBuildPluginFor(nil, "peerlock", "-o=pm-plugC"+pluginSuffix, "./peerlock/plugC")

	os.Setenv("GOPATH", filepath.Join(GOPATH, "alt"))
	if err := os.Chdir(altRoot); err != nil {
		log.Panic(err)
	} else {
		prettyPrintf("cd %s\n", altRoot)
	}
	os.Setenv("PWD", altRoot)
	// Must be For("peerlock"), not hostImplib: PE imports are name-bound,
	// so an implib naming host.exe makes plugB's addmoduledata IAT-call
	// land outside the peerlock.exe process — the DLL loads fine but its
	// moduledata never reaches this host's chain (seen on the VM: chain
	// held only plugA at plugB's validation, and lastmoduleinit fell back
	// to initialized plugA reporting "plugin already loaded"). POSIX is
	// unaffected (dlopen resolves symbols, not module names).
	goCmdBuildPluginFor(nil, "peerlock", "-o="+filepath.Join(modRoot, "pm-plugB"+pluginSuffix), "./peerlock/plugB")

	os.Setenv("GOPATH", GOPATH)
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	} else {
		prettyPrintf("cd %s\n", modRoot)
	}
	os.Setenv("PWD", modRoot)

	// Same-base-name fixture (trendvidia/go#40): two different plugins
	// installed under ONE base file name in two directories, plus the
	// host that opens both. go build -o does not create directories,
	// so make them first.
	goCmd(nil, "build", "-o", "samename.exe", "./samename")
	for _, d := range []string{"same-a", "same-b"} {
		if err := os.MkdirAll(filepath.Join(modRoot, d), 0777); err != nil {
			log.Panic(err)
		}
	}
	goCmdBuildPluginFor(nil, "samename", "-o="+filepath.Join("same-a", "p"+pluginSuffix), "./samename/dira")
	goCmdBuildPluginFor(nil, "samename", "-o="+filepath.Join("same-b", "p"+pluginSuffix), "./samename/dirb")

	// Stage-2 ABI rejection residue: one plugin source built twice, so
	// the rejectchained host can byte-patch one copy's marker to a
	// foreign generation, load it raw, and prove the rejection leaves
	// no walkable chain node behind. The host takes the correct
	// {Marker, Version} pair as hex so nothing is hardcoded.
	goCmd(nil, "build", "-o", "rejectchained.exe", "./rejectchained")
	for _, d := range []string{"reject-a", "reject-b"} {
		if err := os.MkdirAll(filepath.Join(modRoot, d), 0777); err != nil {
			log.Panic(err)
		}
	}
	goCmdBuildPluginFor(nil, "rejectchained", "-o="+filepath.Join("reject-a", "p"+pluginSuffix), "./rejectchained/victim")
	goCmdBuildPluginFor(nil, "rejectchained", "-o="+filepath.Join("reject-b", "p"+pluginSuffix), "./rejectchained/victim")
	// A third, differently-named plugin so the rejection can happen on
	// a chain whose tail is a peer plugin rather than the host.
	goCmdBuildPluginFor(nil, "rejectchained", "-o=reject-peer"+pluginSuffix, "./rejectchained/peer")

	return m.Run()
}

func goCmd(t *testing.T, op string, args ...string) string {
	if t != nil {
		t.Helper()
	}
	var flags []string
	if op != "tool" {
		flags = []string{"-gcflags", gcflags}
	}
	return run(t, filepath.Join(goroot, "bin", "go"), append(append([]string{op}, flags...), args...)...)
}

// hostImplib holds the path to the main host.exe import library,
// used on Windows to link plugins against the host's runtime.
var hostImplib string

// goCmdBuildPlugin is a helper for building plugins that handles
// Windows-specific requirements (plugin suffix, import library).
// The args should be the same as for "go build -buildmode=plugin",
// excluding the "-buildmode=plugin" itself. On Windows, the
// -pluginimplib flag is merged into any existing -ldflags argument,
// pointing at hostImplib (host.exe.a). Tests that run their own host
// binary instead of host.exe must build that host first and call
// goCmdBuildPluginFor with the host's basename instead.
func goCmdBuildPlugin(t *testing.T, args ...string) {
	if t != nil {
		t.Helper()
	}
	goCmdBuildPluginWithImplib(t, hostImplib, args...)
}

// goCmdBuildPluginFor is goCmdBuildPlugin for a test that runs its
// own host binary. hostName is the host's basename without extension
// (e.g. "issue19418" → uses issue19418.exe.a as the import library).
// The host MUST be built before this is called; -buildmode=exe on
// Windows generates the matching .exe.a alongside the binary.
//
// Required on Windows because the plugin's IMAGE_DIRECTORY_ENTRY_IMPORT
// points at a specific host binary by name (PE imports are name-bound),
// so a plugin built against host.exe.a cannot share the runtime of a
// host called issue19418.exe — the OS would load both binaries, the
// plugin's addmoduledata IAT-call would land in host.exe's runtime not
// issue19418.exe's, and issue19418.exe's plugin.Open would see no new
// moduledata in its own chain. Each test's plugin must link against
// the matching host's import library.
//
// Non-Windows is unaffected (POSIX dlopen resolves runtime symbols
// against the loading process at runtime).
func goCmdBuildPluginFor(t *testing.T, hostName string, args ...string) {
	if t != nil {
		t.Helper()
	}
	implib := ""
	if runtime.GOOS == "windows" {
		implib = filepath.Join(filepath.Dir(hostImplib), hostName+".exe.a")
	}
	goCmdBuildPluginWithImplib(t, implib, args...)
}

func goCmdBuildPluginWithImplib(t *testing.T, implib string, args ...string) {
	if t != nil {
		t.Helper()
	}
	if runtime.GOOS == "windows" && implib != "" {
		implibFlag := "-pluginimplib=" + implib
		// Merge the import library flag into existing -ldflags, if any.
		merged := false
		for i, a := range args {
			if strings.HasPrefix(a, "-ldflags=") {
				args[i] = a + " " + implibFlag
				merged = true
				break
			}
		}
		if !merged {
			args = append([]string{"-ldflags=" + implibFlag}, args...)
		}
	}
	goCmd(t, "build", append([]string{"-buildmode=plugin"}, args...)...)
}

// escape converts a string to something suitable for a shell command line.
func escape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	// Conservative guess at characters that will force quoting
	if s == "" || strings.ContainsAny(s, "\\ ;#*&$~?!|[]()<>{}`") {
		s = "'" + s + "'"
	}
	return s
}

// asCommandLine renders cmd as something that could be copy-and-pasted into a command line
func asCommandLine(cwd string, cmd *exec.Cmd) string {
	s := "("
	if cmd.Dir != "" && cmd.Dir != cwd {
		s += "cd" + escape(cmd.Dir) + ";"
	}
	for _, e := range cmd.Env {
		if !strings.HasPrefix(e, "PATH=") &&
			!strings.HasPrefix(e, "HOME=") &&
			!strings.HasPrefix(e, "USER=") &&
			!strings.HasPrefix(e, "SHELL=") {
			s += " "
			s += escape(e)
		}
	}
	// These EVs are relevant to this test.
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PWD=") ||
			strings.HasPrefix(e, "GOPATH=") ||
			strings.HasPrefix(e, "LD_LIBRARY_PATH=") {
			s += " "
			s += escape(e)
		}
	}
	for _, a := range cmd.Args {
		s += " "
		s += escape(a)
	}
	s += " )"
	return s
}

func run(t *testing.T, bin string, args ...string) string {
	cmd := exec.Command(bin, args...)
	cmdLine := asCommandLine(".", cmd)
	prettyPrintf("%s\n", cmdLine)
	cmd.Stderr = new(strings.Builder)
	out, err := cmd.Output()
	if err != nil {
		if t == nil {
			log.Panicf("%#q: %v\n%s", cmd, err, cmd.Stderr)
		} else {
			t.Helper()
			t.Fatalf("%#q: %v\n%s", cmd, err, cmd.Stderr)
		}
	}

	return string(bytes.TrimSpace(out))
}

func TestDWARFSections(t *testing.T) {
	// test that DWARF sections are emitted for plugins and programs importing "plugin"
	globalSkip(t)
	goCmd(t, "run", "./checkdwarf/main.go", "plugin2"+pluginSuffix, "plugin2.UnexportedNameReuse")
	goCmd(t, "run", "./checkdwarf/main.go", "./host.exe", "main.main")
}

func TestBuildID(t *testing.T) {
	// check that plugin has build ID.
	globalSkip(t)
	b := goCmd(t, "tool", "buildid", "plugin1"+pluginSuffix)
	if len(b) == 0 {
		t.Errorf("build id not found")
	}
}

// peExportNames returns the names in image's PE export directory.
// debug/pe does not expose IMAGE_EXPORT_DIRECTORY, so walk it by hand
// through optional-header data directory entry 0, mapping each RVA to
// its containing section. Go plugin images keep the directory and its
// name strings in one section, so all reads below share dirSec.
func peExportNames(t *testing.T, image string) map[string]bool {
	t.Helper()
	f, err := pe.Open(image)
	if err != nil {
		t.Fatalf("pe.Open(%s): %v", image, err)
	}
	defer f.Close()
	var dd pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		dd = oh.DataDirectory[0] // IMAGE_DIRECTORY_ENTRY_EXPORT
	case *pe.OptionalHeader32:
		dd = oh.DataDirectory[0]
	default:
		t.Fatalf("%s: unexpected optional header type %T", image, f.OptionalHeader)
	}
	if dd.VirtualAddress == 0 {
		t.Fatalf("%s: image has no export directory", image)
	}
	secAt := func(rva uint32) *pe.Section {
		for _, s := range f.Sections {
			if rva >= s.VirtualAddress && rva < s.VirtualAddress+max(s.VirtualSize, s.Size) {
				return s
			}
		}
		return nil
	}
	dirSec := secAt(dd.VirtualAddress)
	if dirSec == nil {
		t.Fatalf("%s: export directory RVA %#x not in any section", image, dd.VirtualAddress)
	}
	data, err := dirSec.Data()
	if err != nil {
		t.Fatalf("%s: reading section %s: %v", image, dirSec.Name, err)
	}
	// Every read below is bounds-checked against dirSec's raw bytes.
	// secAt admits RVAs up to max(VirtualSize, Size) while Data() returns
	// only SizeOfRawData bytes, and RVA arithmetic here is unsigned — so
	// an unchecked read of a malformed image either wraps to a huge
	// offset or runs off the end, and panics. A panic escapes t.Fatalf's
	// contract: it takes down the whole testplugin binary and every test
	// after this one, instead of failing this test with a diagnosis.
	offsetIn := func(what string, rva uint32, n int) int {
		if rva < dirSec.VirtualAddress {
			t.Fatalf("%s: %s RVA %#x below section %s base %#x", image, what, rva, dirSec.Name, dirSec.VirtualAddress)
		}
		off := int64(rva) - int64(dirSec.VirtualAddress)
		if off+int64(n) > int64(len(data)) {
			t.Fatalf("%s: %s RVA %#x runs past section %s data (%d bytes)", image, what, rva, dirSec.Name, len(data))
		}
		return int(off)
	}
	u32 := func(what string, rva uint32) uint32 {
		off := offsetIn(what, rva, 4)
		return binary.LittleEndian.Uint32(data[off : off+4])
	}
	numNames := u32("NumberOfNames", dd.VirtualAddress+24) // IMAGE_EXPORT_DIRECTORY.NumberOfNames
	namesArr := u32("AddressOfNames", dd.VirtualAddress+32)
	names := make(map[string]bool, numNames)
	for i := uint32(0); i < numNames; i++ {
		// A corrupt NumberOfNames can overflow this RVA; offsetIn then
		// reports it rather than indexing out of range.
		nameRVA := u32("name pointer", namesArr+4*i)
		off := offsetIn("export name", nameRVA, 1)
		end := bytes.IndexByte(data[off:], 0)
		if end < 0 {
			t.Fatalf("%s: unterminated export name at RVA %#x", image, nameRVA)
		}
		names[string(data[off:off+end])] = true
	}
	return names
}

// elfDynsymNames returns the defined symbol names in image's .dynsym —
// the ELF counterpart of the PE export directory. Undefined entries are
// the image's own imports, not exports.
//
// The two tables are counterparts but not equivalents, and callers must
// anchor their assertions accordingly. A plugin is linked with
// DynlinkingGo() true, which keeps every Go symbol STB_GLOBAL
// (cmd/link/internal/ld/symtab.go putelfsym), so .dynsym holds the whole
// linked program — 9701 defined entries for the mangle fixture on
// linux/arm64, runtime and standard library included — not just the
// package's declared exports. An unanchored substring match against this
// table is therefore close to meaningless; see unnamedPluginPath.
func elfDynsymNames(t *testing.T, image string) map[string]bool {
	t.Helper()
	f, err := elf.Open(image)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", image, err)
	}
	defer f.Close()
	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatalf("%s: no dynamic symbol table: %v", image, err)
	}
	names := make(map[string]bool)
	for _, s := range syms {
		if s.Section != elf.SHN_UNDEF {
			names[s.Name] = true
		}
	}
	return names
}

// unnamedPluginPath recovers the pluginpath of a plugin built from a file
// list rather than a directory. Such a package is unnamed, so cmd/go
// derives its pluginpath by hashing the source files
// ("plugin/unnamed-%x", cmd/go/internal/work/gc.go), which means the test
// cannot know it before the build and has to read it back off the image.
//
// It exists so that expectations can be ANCHORED to the plugin that owns
// them. The export table is not a fixture's own export list; it is every
// symbol linked into the image. Measured on linux/arm64 at 0c08960e25,
// et-mangle.so has 9701 defined .dynsym entries, and the bare fragments
// this replaced matched things the fixture never declared: ".P" matched
// 98 entries including reflect.(*sliceType).Pointers, ".G" matched 115
// including runtime.Gosched. The proof they pinned nothing: run those two
// fragments against et-plugin1.so, which declares neither P nor G, and
// they still match 70 and 84 entries. Only ".F[" was self-anchoring.
//
// Fatalf rather than a sentinel: if the pluginpath shape ever changes,
// silently failing to find it would send every dependent assertion back
// to matching nothing in particular, which is the defect this removes.
func unnamedPluginPath(t *testing.T, image string, table map[string]bool) string {
	t.Helper()
	const prefix = "plugin/unnamed-"
	var paths []string
	seen := make(map[string]bool)
	for name := range table {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// name is <pluginpath>.<something>; the hash runs to the first dot.
		rest := name[len(prefix):]
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			continue
		}
		if path := prefix + rest[:i]; !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	if len(paths) != 1 {
		slices.Sort(paths)
		t.Fatalf("%s: want exactly one %q pluginpath in export table, found %d: %q (%d entries present)",
			image, prefix, len(paths), paths, len(table))
	}
	return paths[0]
}

// TestExportTableCompleteness pins the export mechanism's wire contract
// (trendvidia/go#44): every symbol a plugin main package declares for
// export must appear in the linked image's dynamic export table — the
// PE export directory on Windows, .dynsym on ELF.
//
// Two layers:
//
//  1. Loader layer (exporttab.exe): on every platform, plugin.Open
//     resolves each ptabs symbol through the dynamic symbol table —
//     dlsym on POSIX (plugin_dlopen.go), GetProcAddress on Windows —
//     so an export missing from the linked image aborts the open with
//     a named error.
//
//  2. Static layer: this test parses each image directly and requires
//     the declared export set to be a subset of its export table,
//     without loading anything. That pins completeness at build-test
//     time against the source-level declaration, lets CI judge an
//     artifact it has not loaded, and holds even if the loader-side
//     resolution strategy changes underneath it.
//
// What layer 2 pins differs by platform, and the ELF leg is the weaker
// of the two. The PE export directory is built by the export path
// itself, so the Windows leg pins that path directly. ELF .dynsym is
// not: it carries every Go symbol in the image (see elfDynsymNames), so
// the POSIX leg pins that each declared export survived deadcode and
// kept global binding — a real regression net, but not evidence that
// the go:plugin.exports/dynexp machinery ran. The loader layer covers
// that on both.
//
// Darwin is skipped: Mach-O carries exports in the dyld export trie,
// which these parsers do not cover (gap noted on trendvidia/go#44).
func TestExportTableCompleteness(t *testing.T) {
	globalSkip(t)
	if runtime.GOOS == "darwin" {
		t.Skip("Mach-O dyld export trie not parsed; see trendvidia/go#44")
	}

	goCmd(t, "build", "-o", "exporttab.exe", "./exporttab")

	// One fixture per export class that history or linkage makes
	// interesting: plugin1 covers plain funcs plus a zero-initialized
	// BSS var (the storage class mingw ld's --export-all-symbols
	// auto-export once silently skipped); sub/plugin1 repeats a base
	// name another fixture also uses and adds a func-valued var;
	// mangle exercises generic and mangled export names.
	//
	// Every expectation is a declared name qualified by its owning
	// pluginpath, never a bare fragment: the table is the whole linked
	// program, so an unanchored match proves nothing (elfDynsymNames,
	// unnamedPluginPath). Directory builds derive the pluginpath from
	// the package import path ("testplugin/..."); the file-list build is
	// an unnamed package whose path embeds a source hash, so it leaves
	// path empty and the test recovers it from the image.
	fixtures := []struct {
		src  string
		out  string
		path string // owning pluginpath; "" means unnamed, so discover it

		// Names declared for export by the fixture source. Each is
		// required in the table verbatim, as path+"."+name.
		exports []string

		// Generic funcs declared by the fixture source. Measured on
		// linux/arm64 (2026-08): these get no table entry under their
		// own name — Lookup("F") fails on POSIX too, which is
		// consistent compiler filtering rather than a dropped export.
		// Only instantiations appear, under mangled names such as
		// "F[C/NhSSEp]", so each is required as some entry prefixed
		// path+"."+name+"[".
		generics []string
	}{
		{
			src:     "./plugin1",
			out:     "et-plugin1",
			path:    "testplugin/plugin1",
			exports: []string{"F", "ReadCommonX", "Seven", "UnexportedNameReuse"},
		},
		{
			src:     "./sub/plugin1",
			out:     "et-sub-plugin1",
			path:    "testplugin/sub/plugin1",
			exports: []string{"F", "FuncVar", "ReadCommonX"},
		},
		{
			// File argument, matching TestSymbolNameMangle: a directory
			// build resolves this package against the module's implicit
			// -lang=go1.16 and rejects the generics below. Building from
			// a file list is also what makes it an unnamed package.
			src:      "./mangle/plugin.go",
			out:      "et-mangle",
			exports:  []string{"P", "G"},
			generics: []string{"F"},
		},
	}
	for _, fx := range fixtures {
		goCmdBuildPluginFor(t, "exporttab", "-o="+fx.out+pluginSuffix, fx.src)
	}

	// Loader layer: open everything in one host process.
	paths := make([]string, len(fixtures))
	for i, fx := range fixtures {
		paths[i] = fx.out + pluginSuffix
	}
	out := run(t, "./exporttab.exe", paths...)
	for _, p := range paths {
		if !strings.Contains(out, "OPENED "+p) {
			t.Errorf("host did not report opening %s:\n%s", p, out)
		}
	}

	// Static layer: parse each image and require the declared set.
	for _, fx := range fixtures {
		image := fx.out + pluginSuffix
		// A fixture at its zero value builds, loads and parses like any
		// other and then requires nothing of the result, so it reports
		// success having pinned nothing. Both loops below are no-ops on
		// empty slices, which makes that failure completely silent.
		if len(fx.exports) == 0 && len(fx.generics) == 0 {
			t.Errorf("%s: fixture declares no exports and no generics, so it pins nothing", image)
			continue
		}
		var table map[string]bool
		if runtime.GOOS == "windows" {
			table = peExportNames(t, image)
		} else {
			table = elfDynsymNames(t, image)
		}
		path := fx.path
		if path == "" {
			path = unnamedPluginPath(t, image, table)
		}
		for _, name := range fx.exports {
			if want := path + "." + name; !table[want] {
				t.Errorf("%s: export %q missing from image export table (%d entries present)", image, want, len(table))
			}
		}
		for _, name := range fx.generics {
			want := path + "." + name + "["
			found := false
			for entry := range table {
				if strings.HasPrefix(entry, want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: generic %q has no instantiation in image export table: no entry with prefix %q (%d entries present)", image, name, want, len(table))
			}
		}
	}
}

func TestRunHost(t *testing.T) {
	globalSkip(t)
	run(t, "./host.exe")
}

// TestCloseAPI exercises the L3.b additions: Plugin.Close,
// Plugin.HasDependents, Plugin.Dependents.
func TestCloseAPI(t *testing.T) {
	globalSkip(t)
	run(t, "./closeapi.exe")
}

// TestPluginABIMarker pins the cross-toolchain ABI fence
// (trendvidia/go#24 part 2): on Windows, plugin.Open must reject a
// candidate DLL whose go:plugin.abi export is absent or names another
// ABI generation. The check reads static file bytes before LoadLibrary
// maps anything, so both negative cases are exercised in-process — no
// foreign code runs, and nothing touches this process's module chain.
// Positive control is structural only (marker bytes present in a
// freshly built plugin); full load success is covered by every other
// test in this suite, which would fail if the fence rejected good
// binaries.
func TestPluginABIMarker(t *testing.T) {
	globalSkip(t)
	if runtime.GOOS != "windows" {
		t.Skip("ABI marker fence is Windows-only")
	}

	src, err := os.ReadFile("plugin1" + pluginSuffix)
	if err != nil {
		t.Fatal(err)
	}
	// Locate the marker by the full 8-byte {Marker, Version} pair the
	// linker emits, and require it to be unique. The 4-byte Marker
	// alone is a sequence a multi-megabyte image hits by coincidence
	// with non-trivial probability, and bytes.Index takes the FIRST
	// occurrence — .text and pclntab precede .edata in file order — so
	// searching for it could deterministically tamper with unrelated
	// bytes, leave the real marker intact, and report the resulting
	// unrelated load failure as "want incompatible-ABI rejection".
	magic := make([]byte, 8)
	binary.LittleEndian.PutUint32(magic, pluginabi.Marker)
	binary.LittleEndian.PutUint32(magic[4:], pluginabi.Version)
	idx := bytes.Index(src, magic)
	if idx < 0 {
		t.Fatal("freshly built plugin lacks the go:plugin.abi marker")
	}
	if dup := bytes.Index(src[idx+1:], magic); dup >= 0 {
		t.Fatalf("go:plugin.abi marker bytes occur at both %d and %d; tampering would be ambiguous", idx, idx+1+dup)
	}

	// Wrong generation: bump Version past ours; Open must refuse.
	bad := append([]byte(nil), src...)
	binary.LittleEndian.PutUint32(bad[idx+4:], pluginabi.Version+1)
	if err := os.WriteFile("abi-mismatch"+pluginSuffix, bad, 0444); err != nil {
		t.Fatal(err)
	}
	_, err = plugin.Open("abi-mismatch" + pluginSuffix)
	if err == nil {
		t.Fatal("Open accepted a plugin from a different toolchain ABI")
	}
	if !strings.Contains(err.Error(), "incompatible toolchain ABI") {
		t.Fatalf("error = %q; want incompatible-ABI rejection", err)
	}

	// Absent marker: rename the export out of the directory entirely.
	noabi := bytes.ReplaceAll(src, []byte("go:plugin.abi"), []byte("xo:plugin.abi"))
	if err := os.WriteFile("abi-missing"+pluginSuffix, noabi, 0444); err != nil {
		t.Fatal(err)
	}
	_, err = plugin.Open("abi-missing" + pluginSuffix)
	if err == nil {
		t.Fatal("Open accepted a plugin without an ABI marker")
	}
	if !strings.Contains(err.Error(), "missing go:plugin.abi") {
		t.Fatalf("error = %q; want missing-marker rejection", err)
	}
}

// TestSameBaseNameDistinctDirs pins the post-load half of the ABI
// fence (trendvidia/go#40): two DIFFERENT plugins sharing one base
// file name must never collapse onto one OS module. The platform
// loader's matching rule decides which good outcome occurs — B loads
// as its own module (measured on Windows 11 arm64 and on POSIX), or
// the second Open is refused with the named "already loaded from"
// rejection — and the fixture fails on the forbidden third outcome: a
// successful open whose symbols answer A.
func TestSameBaseNameDistinctDirs(t *testing.T) {
	globalSkip(t)
	run(t, "./samename.exe")
}

// TestRejectChainedModule pins the stage-2 ABI rejection's residue
// handling on a REAL chained module: the host byte-patches a healthy
// plugin's marker to a foreign generation and loads it raw, so its
// DllMain chains its moduledata exactly as a swapped-in incompatible
// build would, then verifies the rejection treatment detaches that
// node and that a healthy sibling still loads afterwards. The host
// prints skip on platforms without the Windows loader half.
func TestRejectChainedModule(t *testing.T) {
	globalSkip(t)
	var pair [8]byte
	binary.LittleEndian.PutUint32(pair[0:], pluginabi.Marker)
	binary.LittleEndian.PutUint32(pair[4:], pluginabi.Version)
	run(t, "./rejectchained.exe", hex.EncodeToString(pair[:]))
}

// TestPanicOnCallStub exercises v2's funcptr panic-on-call stub
// conversion: a func value obtained from plugin.Lookup must, after
// Plugin.Close, panic with a recognisable message rather than jumping
// into unmapped plugin text. The fixture loads plugin1.so, takes a
// reference to plugin1.F, closes the plugin, then invokes the held
// reference under recover() and checks the panic message.
func TestPanicOnCallStub(t *testing.T) {
	globalSkip(t)
	run(t, "./panicstub.exe")
}

// TestLookupDuringClose races concurrent Lookup calls against Close
// (trendvidia/go#23). lookup() must serialise p.syms/p.pluginpath
// access on pluginsMu; the fixture hammers Lookup from four goroutines
// while the main goroutine closes, then checks every outcome: results
// are successes or clean errors only, sampled func values panic via
// the closed-plugin stub when called afterwards, and a second Close
// is an idempotent no-op. Under a -race build of this suite, the
// pre-fix unlocked map read is reported as a data race.
func TestLookupDuringClose(t *testing.T) {
	globalSkip(t)
	run(t, "./lookuprace.exe")
}

// TestCloseUnloadsMemory verifies that v2's Plugin.Close actually
// unloads the plugin and the kernel unmaps its text pages. The
// fixture captures the looked-up code address before Close, calls
// Close, then probes the page: POSIX uses mincore (ENOMEM proves
// the mapping is gone); Windows uses VirtualQuery (MEM_FREE proves
// FreeLibrary released the image). TestPanicOnCallStub on its own
// only shows that the descriptor rewrite ran before unmap; this
// test shows that the unmap also happened.
func TestCloseUnloadsMemory(t *testing.T) {
	globalSkip(t)
	run(t, "./closeunmaps.exe")
}

// runExpectFault runs bin, requiring the child to die from an
// unrecoverable runtime fault: the probe holds a reference class
// Plugin.Close does not protect, and using it after Close must crash
// the process ("fatal error: fault") rather than return. A clean exit
// fails, as does a death without the fault marker — in particular a
// recoverable "panic:" exit would mean the stub rewrite over-reached
// onto an unprotected class.
func runExpectFault(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	prettyPrintf("%s\n", asCommandLine(".", cmd))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("%s %s: survived a post-Close use of an unprotected reference; want fatal error: fault\nstdout:\n%s",
			bin, strings.Join(args, " "), out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("%#q: %v (want exit from runtime fault)\nstderr:\n%s", cmd, err, stderr.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, "fatal error: fault") {
		t.Fatalf("%s %s: died without \"fatal error: fault\"; want unrecoverable fault, got:\n%s",
			bin, strings.Join(args, " "), msg)
	}
}

// TestCloseHazardEnvelope pins the documented post-Close reference
// envelope (trendvidia/go#26). Func values from Lookup panic
// recoverably after Close — that class is covered by
// TestPanicOnCallStub. The three unprotected classes must behave as
// documented:
//
//   - a closure obtained by calling into the plugin kills the process
//     with an unrecoverable fault when called after Close (the stub
//     rewrite cannot reach its descriptor),
//
//   - a pointer to an exported variable likewise faults on use, and
//     function values reached through such closures share the same
//     call-into-unmapped-text mechanism,
//
//   - if the path is reopened instead, stale references silently
//     alias the NEW instance because the loader maps it at the same
//     addresses.
//
// The two fatal classes each run in their own subprocess via
// runExpectFault; the aliasing cell runs in-process-safe subprocess
// form too, so a future loader that stops reusing addresses turns the
// test failure into reported output instead of a dead test binary.
func TestCloseHazardEnvelope(t *testing.T) {
	globalSkip(t)
	runExpectFault(t, "./hazards.exe", "closure")
	runExpectFault(t, "./hazards.exe", "dataptr")
	run(t, "./hazards.exe", "alias")
}

// TestPeerVersionLock exercises the peer-plugin half of the version
// lock (trendvidia/go#24). peerlock/peerlib is linked into the plugins
// but not into the host, so host-side validation never sees it:
// pm-plugA loads first and owns the package; pm-plugB carries a
// divergent build of the same import path and must be REJECTED with
// the standard named-package error rather than silently binding to A's
// instance; pm-plugC (same build as A) is the positive control and
// must load and share. On POSIX the runtimehash-pointer interposition
// of plugin_lastmoduleinit already fences this; on Windows only the
// explicit peer scan in validatePkghashes does.
func TestPeerVersionLock(t *testing.T) {
	globalSkip(t)
	run(t, "./peerlock.exe")
}

// TestRefcountUnload exercises the cross-plugin dependency tracking
// added by L3.a. plugA and plugB both import a package the host
// does not (refcount/mylib), so plugA owns the canonical
// mylib.Counter and plugB's slots canonicalise to plugA's instance.
// The test verifies that Counter is shared (canonicalisation
// works), that plugA reports plugB as a dependent, that Close on
// plugA fails while plugB depends on it, and that the order
// plugB→plugA Close succeeds.
func TestRefcountUnload(t *testing.T) {
	globalSkip(t)
	run(t, "./refcount.exe")
}

func TestUniqueTypesAndItabs(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "iface.exe", "./iface")
	goCmdBuildPluginFor(t, "iface", "./iface_a")
	goCmdBuildPluginFor(t, "iface", "./iface_b")
	run(t, "./iface.exe")
}

func TestIssue18676(t *testing.T) {
	// make sure we don't add the same itab twice.
	// The buggy code hangs forever, so use a timeout to check for that.
	globalSkip(t)
	goCmd(t, "build", "-o", "issue18676.exe", "./issue18676/main.go")
	goCmdBuildPluginFor(t, "issue18676", "-o", "plugin"+pluginSuffix, "./issue18676/plugin.go")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "./issue18676.exe")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%#q: %v\n%s", cmd, err, out)
	}
}

func TestIssue19534(t *testing.T) {
	// Test that we can load a plugin built in a path with non-alpha characters.
	globalSkip(t)
	goCmd(t, "build", "-o", "issue19534.exe", "./issue19534/main.go")
	goCmdBuildPluginFor(t, "issue19534", "-gcflags=-p=issue.19534", "-ldflags=-pluginpath=issue.19534", "-o", "plugin"+pluginSuffix, "./issue19534/plugin.go")
	run(t, "./issue19534.exe")
}

func TestIssue18584(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue18584.exe", "./issue18584/main.go")
	goCmdBuildPluginFor(t, "issue18584", "-o", "plugin"+pluginSuffix, "./issue18584/plugin.go")
	run(t, "./issue18584.exe")
}

func TestIssue19418(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue19418.exe", "./issue19418/main.go")
	goCmdBuildPluginFor(t, "issue19418", "-ldflags=-X main.Val=linkstr", "-o", "plugin"+pluginSuffix, "./issue19418/plugin.go")
	run(t, "./issue19418.exe")
}

func TestIssue19529(t *testing.T) {
	globalSkip(t)
	goCmdBuildPlugin(t, "-o", "plugin"+pluginSuffix, "./issue19529/plugin.go")
}

func TestIssue22175(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue22175.exe", "./issue22175/main.go")
	goCmdBuildPluginFor(t, "issue22175", "-o", "issue22175_plugin1"+pluginSuffix, "./issue22175/plugin1.go")
	goCmdBuildPluginFor(t, "issue22175", "-o", "issue22175_plugin2"+pluginSuffix, "./issue22175/plugin2.go")
	run(t, "./issue22175.exe")
}

func TestIssue22295(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue22295.exe", "./issue22295.pkg/main.go")
	goCmdBuildPluginFor(t, "issue22295", "-o", "issue.22295"+pluginSuffix, "./issue22295.pkg")
	run(t, "./issue22295.exe")
}

func TestIssue24351(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue24351.exe", "./issue24351/main.go")
	goCmdBuildPluginFor(t, "issue24351", "-o", "issue24351"+pluginSuffix, "./issue24351/plugin.go")
	run(t, "./issue24351.exe")
}

func TestIssue25756(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue25756.exe", "./issue25756/main.go")
	goCmdBuildPluginFor(t, "issue25756", "-o", "life"+pluginSuffix, "./issue25756/plugin")
	// Fails intermittently, but 20 runs should cause the failure
	for n := 20; n > 0; n-- {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			run(t, "./issue25756.exe")
		})
	}
}

// Test with main using -buildmode=pie with plugin for issue #43228
func TestIssue25756pie(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-buildmode=pie", "-o", "issue25756pie.exe", "./issue25756/main.go")
	goCmdBuildPluginFor(t, "issue25756pie", "-o", "life"+pluginSuffix, "./issue25756/plugin")
	run(t, "./issue25756pie.exe")
}

func TestMethod(t *testing.T) {
	// Exported symbol's method must be live.
	globalSkip(t)
	goCmd(t, "build", "-o", "method.exe", "./method/main.go")
	goCmdBuildPluginFor(t, "method", "-o", "plugin"+pluginSuffix, "./method/plugin.go")
	run(t, "./method.exe")
}

func TestMethod2(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "method2.exe", "./method2/main.go")
	goCmdBuildPluginFor(t, "method2", "-o", "method2"+pluginSuffix, "./method2/plugin.go")
	run(t, "./method2.exe")
}

func TestMethod3(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "method3.exe", "./method3/main.go")
	goCmdBuildPluginFor(t, "method3", "-o", "method3"+pluginSuffix, "./method3/plugin.go")
	run(t, "./method3.exe")
}

func TestIssue44956(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue44956.exe", "./issue44956/main.go")
	goCmdBuildPluginFor(t, "issue44956", "-o", "issue44956p1"+pluginSuffix, "./issue44956/plugin1.go")
	goCmdBuildPluginFor(t, "issue44956", "-o", "issue44956p2"+pluginSuffix, "./issue44956/plugin2.go")
	run(t, "./issue44956.exe")
}

func TestIssue52937(t *testing.T) {
	globalSkip(t)
	goCmdBuildPlugin(t, "-o", "issue52937"+pluginSuffix, "./issue52937/main.go")
}

func TestIssue53989(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue53989.exe", "./issue53989/main.go")
	goCmdBuildPluginFor(t, "issue53989", "-o", "issue53989"+pluginSuffix, "./issue53989/plugin.go")
	run(t, "./issue53989.exe")
}

func TestForkExec(t *testing.T) {
	// Issue 38824: importing the plugin package causes it hang in forkExec on darwin.
	globalSkip(t)

	t.Parallel()
	goCmd(t, "build", "-o", "forkexec.exe", "./forkexec/main.go")

	for i := 0; i < 100; i++ {
		cmd := testenv.Command(t, "./forkexec.exe", "1")
		err := cmd.Run()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				t.Logf("stderr:\n%s", ee.Stderr)
			}
			t.Errorf("running command failed: %v", err)
			break
		}
	}
}

func TestSymbolNameMangle(t *testing.T) {
	// Issue 58800: generic function name may contain weird characters
	// that confuse the external linker.
	// Issue 62098: the name mangling code doesn't handle some string
	// symbols correctly.
	globalSkip(t)
	goCmdBuildPlugin(t, "-o", "mangle"+pluginSuffix, "./mangle/plugin.go")
}

func TestIssue62430(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "issue62430.exe", "./issue62430/main.go")
	goCmdBuildPluginFor(t, "issue62430", "-o", "issue62430"+pluginSuffix, "./issue62430/plugin.go")
	run(t, "./issue62430.exe")
}

func TestTextSectionSplit(t *testing.T) {
	globalSkip(t)
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("text section splitting is not done in %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	// Use -ldflags=-debugtextsize=262144 to let the linker split text section
	// at a smaller size threshold, so it actually splits for the test binary.
	goCmd(nil, "build", "-ldflags=-debugtextsize=262144", "-o", "host-split.exe", "./host")
	run(t, "./host-split.exe")

	// Check that we did split text sections.
	syms := goCmd(nil, "tool", "nm", "host-split.exe")
	if !strings.Contains(syms, "runtime.text.1") {
		t.Errorf("runtime.text.1 not found, text section not split?")
	}
}

func TestIssue67976(t *testing.T) {
	// Issue 67976: build failure with loading a dynimport variable (the runtime/pprof
	// package does this on darwin) in a plugin on darwin/amd64.
	// The test program uses runtime/pprof in a plugin.
	globalSkip(t)
	goCmdBuildPlugin(t, "-o", "issue67976"+pluginSuffix, "./issue67976/plugin.go")
}

func TestIssue75102(t *testing.T) {
	globalSkip(t)
	// add gcflags different from the executable file to trigger plugin open failed.
	goCmd(t, "build", "-o", "issue75102.exe", "./issue75102/main.go")
	goCmdBuildPluginFor(t, "issue75102", "-gcflags=all=-N -l", "-o", "issue75102"+pluginSuffix, "./issue75102/plugin.go")
	run(t, "./issue75102.exe")
}
