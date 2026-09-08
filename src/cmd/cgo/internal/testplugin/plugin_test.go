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
	// The godomain fixture needs a shared package whose import path
	// lives under a go.*-style module domain (trendvidia/go#108), which
	// no path inside module testplugin can spell. The gofakemod
	// directory carries its own go.mod (module go.example.org/fakemod);
	// point the main module at it. The rootmod fixture needs the
	// neighbouring shape — a package at the ROOT of a single-element
	// dotted module path, whose symbols carry no slash at all
	// (trendvidia/go#110) — and a module root holds exactly one
	// package, so its two halves (plugin-only and host-shared) need one
	// module each: rootmodcom is `module example.com`, rootmodorg is
	// `module example.org`. Only modRoot gets the requires — altRoot
	// has no copy of the replacement directories, so it keeps the bare
	// `module testplugin`. Each root's go.mod is written exactly once,
	// so the replayable transcript below shows one command per file
	// rather than a write followed by a contradicting rewrite.
	goMods := map[string]string{
		modRoot: "module testplugin\n\ngo 1.26\n\nrequire (\n\texample.com v0.0.0\n\texample.org v0.0.0\n\textensions v0.0.0\n\tgo.example.org/fakemod v0.0.0\n)\n\nreplace (\n\texample.com => ./rootmodcom\n\texample.org => ./rootmodorg\n\textensions => ./dotlessmod\n\tgo.example.org/fakemod => ./gofakemod\n)\n",
		altRoot: "module testplugin\n\ngo 1.26\n",
	}
	for srcRoot, dstRoot := range map[string]string{
		"testdata":                           modRoot,
		filepath.Join("altpath", "testdata"): altRoot,
	} {
		if err := cgotest.OverlayDir(dstRoot, srcRoot); err != nil {
			log.Panic(err)
		}
		prettyPrintf("mkdir -p %s\n", dstRoot)
		prettyPrintf("rsync -a %s/ %s\n", srcRoot, dstRoot)

		if err := os.WriteFile(filepath.Join(dstRoot, "go.mod"), []byte(goMods[dstRoot]), 0666); err != nil {
			log.Panic(err)
		}
		// The content is printf's ARGUMENT, never its format: a `%` in
		// a go.mod (this fixture's whole subject is %2e-escaped module
		// domains) would otherwise make the replay write different
		// bytes than the harness did. %b, not %s, because %q renders
		// the newlines as `\n` and only %b expands escapes in an
		// argument.
		prettyPrintf("printf '%%b' %q > %s/go.mod\n", goMods[dstRoot], dstRoot)
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

	// Race fixtures: host and plugin are both built -race where the
	// platform supports it (see raceFlags), so an unsynchronised
	// read in package plugin is reported as a DATA RACE (exit 66)
	// instead of passing by timing. goCmd alone never adds -race,
	// and instrumenting this harness would only instrument the
	// exec'ing test binary, not the fixture it runs.
	goCmd(nil, "build", append(raceFlags(), "-o", "lookuprace.exe", "./lookuprace")...)
	goCmdBuildPluginFor(nil, "lookuprace", append(raceFlags(), "-o=lookuprace-plugin1"+pluginSuffix, "./plugin1")...)

	goCmd(nil, "build", append(raceFlags(), "-o", "depsrace.exe", "./depsrace")...)
	goCmdBuildPluginFor(nil, "depsrace", append(raceFlags(), "-o=depsrace-plugin1"+pluginSuffix, "./plugin1")...)
	// Second plugin: its pluginpath is the same length as plugin1's,
	// which is what makes findPluginModule's comparison dereference a
	// dangling snapshot after plugin1 closes. See the fixture's
	// package comment.
	goCmdBuildPluginFor(nil, "depsrace", append(raceFlags(), "-o=depsrace-plugin2"+pluginSuffix, "./plugin2")...)

	// Mid-load failure fixture: the pluginpath override diverges from
	// the compile-time package prefix on purpose, so every symbol
	// lookup in open()'s fill-in loop fails and the could-not-find-
	// symbol path runs. See testdata/lookupfail's package comment.
	goCmd(nil, "build", "-o", "lookupfail.exe", "./lookupfail")
	goCmdBuildPluginFor(nil, "lookupfail", "-ldflags=-pluginpath=lookupfail.mismatch", "-o=lookupfail-plugin"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "zeroclose.exe", "./zeroclose")
	goCmdBuildPluginFor(nil, "zeroclose", "-o=zeroclose-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "lookupcopy.exe", "./lookupcopy")
	goCmdBuildPluginFor(nil, "lookupcopy", "-o=lookupcopy-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "closeunmaps.exe", "./closeunmaps")
	goCmdBuildPluginFor(nil, "closeunmaps", "-o=closeunmaps-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "gcafterclose.exe", "./gcafterclose")
	goCmdBuildPluginFor(nil, "gcafterclose", "-o=gcafterclose-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "refcount.exe", "./refcount/host")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugA"+pluginSuffix, "./refcount/plugA")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugB"+pluginSuffix, "./refcount/plugB")

	// go.*-domain module fixture (trendvidia/go#108): the shared
	// packages live under go.example.org/fakemod, which the blanket
	// `go.` prefix arm used to exclude from data canonicalisation.
	goCmd(nil, "build", "-o", "godomain.exe", "./godomain/host")
	goCmdBuildPluginFor(nil, "godomain", "-o=godomain-plugA"+pluginSuffix, "./godomain/plugA")
	goCmdBuildPluginFor(nil, "godomain", "-o=godomain-plugB"+pluginSuffix, "./godomain/plugB")

	// Cross-module func VALUE identity fixture (trendvidia/go#106):
	// the host links funcident/shared so Fn's body is host-shared,
	// while each plugin emits its own `shared.Fn·f` and a plugin-only
	// package statically initializes a word pointing at it.
	goCmd(nil, "build", "-o", "funcident.exe", "./funcident/host")
	goCmdBuildPluginFor(nil, "funcident", "-o=funcident-plugA"+pluginSuffix, "./funcident/plugA")
	goCmdBuildPluginFor(nil, "funcident", "-o=funcident-plugB"+pluginSuffix, "./funcident/plugB")

	// Root-of-dotted-module-path fixture (trendvidia/go#110): the
	// shared packages sit at the roots of `module example.com` and
	// `module example.org`, so PathToPrefix escapes their dots whole
	// and the globals are named `example%2ecom.Counter` /
	// `example%2eorg.Value` — no slash, which the "no slash means
	// stdlib" arm used to reject.
	goCmd(nil, "build", "-o", "rootmod.exe", "./rootmod/host")
	goCmdBuildPluginFor(nil, "rootmod", "-o=rootmod-plugA"+pluginSuffix, "./rootmod/plugA")
	goCmdBuildPluginFor(nil, "rootmod", "-o=rootmod-plugB"+pluginSuffix, "./rootmod/plugB")

	// Slashless-name fixture (trendvidia/go#115): `module extensions`
	// has no dot to escape and no slash, so its root package is spelled
	// exactly like a stdlib top-level name. The same fixture covers the
	// other half of that gate — a slashless STDLIB package the plugins
	// link and the host does not.
	goCmd(nil, "build", "-o", "dotless.exe", "./dotless/host")
	goCmdBuildPluginFor(nil, "dotless", "-o=dotless-plugA"+pluginSuffix, "./dotless/plugA")
	goCmdBuildPluginFor(nil, "dotless", "-o=dotless-plugB"+pluginSuffix, "./dotless/plugB")

	// Escaped-symbol-name fixture (trendvidia/go#116): host and plugin
	// share go.example.org/fakemod/yaml.v2, whose final path element
	// carries a dot, so PathToPrefix escapes it and every symbol of the
	// package is spelled `...yaml%2ev2.X`. Reading that '%' as a
	// stdcall decoration failed the plugin link outright.
	goCmd(nil, "build", "-o", "escname.exe", "./escname/host")
	goCmdBuildPluginFor(nil, "escname", "-o=escname-plugin"+pluginSuffix, "./escname/plug")

	// Plugin-local ABI0 assembly fixture (trendvidia/go#117): the host
	// links no crypto, so the plugin's own text makes the ABI0 calls
	// and carries the g-reload the windows/amd64 rewrite turns into a
	// load through __imp_runtime.tls_g.
	goCmd(nil, "build", "-o", "asmplugin.exe", "./asmplugin/host")
	goCmdBuildPluginFor(nil, "asmplugin", "-o=asmplugin-plugin"+pluginSuffix, "./asmplugin/plug")

	// Host-shared inittask / plugin-local data fixture
	// (trendvidia/go#74): the host links internal/poll through os but
	// not net, so internal/poll's task is published while net's init
	// stays plugin-local and calls into what that task assigned.
	goCmd(nil, "build", "-o", "internalinit.exe", "./internalinit/host")
	goCmdBuildPluginFor(nil, "internalinit", "-o=internalinit-plugin"+pluginSuffix, "./internalinit/plug")

	// Post-Close hazard-envelope fixture (trendvidia/go#26): one host
	// with subcommand-selected probes over an unprotected closure, an
	// exported-variable pointer, and the reopen-aliasing cell.
	// Extension-upgrade fixture (trendvidia/go#122): v1 and v2 lay
	// their exported fields out differently on purpose, so a pointer
	// held across the upgrade lands on the WRONG field of the new
	// image unless the freed range is reserved.
	goCmd(nil, "build", "-o", "upgrade.exe", "./upgrade/host")
	goCmdBuildPluginFor(nil, "upgrade", "-o=upgrade-v1"+pluginSuffix, "./upgrade/v1")
	goCmdBuildPluginFor(nil, "upgrade", "-o=upgrade-v2"+pluginSuffix, "./upgrade/v2")

	// The other half of the upgrade story (trendvidia/go#122): an
	// extension that follows the contract Plugin.Close documents,
	// upgraded with nothing unsafe to release first.
	goCmd(nil, "build", "-o", "upgradesafe.exe", "./upgradesafe/host")
	goCmdBuildPluginFor(nil, "upgradesafe", "-o=upgradesafe-v1"+pluginSuffix, "./upgradesafe/v1")
	goCmdBuildPluginFor(nil, "upgradesafe", "-o=upgradesafe-v2"+pluginSuffix, "./upgradesafe/v2")

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

	// Alias-path fixture (trendvidia/go#46): one plugin plus a hardlink
	// spelling of it, so the loader resolves the second Open onto the
	// already-loaded module. The plugin reuses samename/dira — the
	// fixture's only observable is that the reopened alias answers.
	goCmd(nil, "build", "-o", "aliaspath.exe", "./aliaspath")
	goCmdBuildPluginFor(nil, "aliaspath", "-o=aliaspath-plugin"+pluginSuffix, "./samename/dira")

	// Static-data canonicalization fixture (trendvidia/go#25): host and
	// plugin both link testplugin/canonicaldata/shared; the plugin
	// embeds descriptors, itabs, and data addresses in static data
	// through each of the issue's four edges, and the host checks the
	// resulting values against its own canonical copies.
	goCmd(nil, "build", "-o", "canonicaldata.exe", "./canonicaldata")
	goCmdBuildPluginFor(nil, "canonicaldata", "-o=canonicaldata-plugin"+pluginSuffix, "./canonicaldata/plugin")

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

// raceFlags returns the build flags that instrument a fixture with
// the race detector, or nil where this platform cannot: the detector
// itself is platform-gated, and outside darwin/linux its runtime
// needs cgo. Fixtures built with these flags still run — and still
// test their behavioural assertions — when the result is nil; only
// the data-race reporting is lost.
func raceFlags() []string {
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		return nil
	}
	// macOS is the only exemption cmd/go grants: see
	// cmd/go/internal/work/init.go ("On macOS, -race does not
	// require cgo"), where anything else without cgo exits 2 with
	// "-race requires cgo" — which here would be a goCmd panic
	// taking the whole package down, not a skipped fixture.
	if !testenv.HasCGO() && runtime.GOOS != "darwin" {
		return nil
	}
	return []string{"-race"}
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

// TestAliasPathRejectionNotCached pins trendvidia/go#46: the alias
// rejection (a requested path resolving onto an already-loaded module)
// is process state, not a property of the file, so plugin.Open must
// not cache it. The fixture opens a plugin, opens a hardlink spelling
// of it twice (both rejected with the named alias error, neither via
// "(previous failure)"), closes the original, and then opens the
// hardlink spelling again — which must now map its own image and
// succeed. Windows-only: the branch exists because the Windows loader
// resolves a request onto an already-mapped module; the POSIX open
// path has no alias branch at all.
func TestAliasPathRejectionNotCached(t *testing.T) {
	globalSkip(t)
	// The fixture bows out on its own when its premise is absent — a
	// non-Windows build, or a loader that did not resolve the hardlink
	// onto the already-mapped image. Report that as a SKIP rather than
	// letting a run that pinned nothing come back green.
	if out := run(t, "./aliaspath.exe"); strings.HasPrefix(out, "skip:") {
		t.Skip(strings.TrimSpace(out))
	}
}

// The TestCanonical* quartet pins trendvidia/go#25: type descriptors,
// itabs, and data addresses that the PLUGIN embedded in static data —
// where no slot-routed code access can canonicalize them — must still
// resolve onto the host's canonical copies at plugin load. One edge of
// the issue's checklist per test; the fixture host asserts each
// plugin-produced value against its own copy of the shared package.

func TestCanonicalDictionaries(t *testing.T) {
	globalSkip(t)
	run(t, "./canonicaldata.exe", "dict")
}

func TestCanonicalStaticIfaceGlobals(t *testing.T) {
	globalSkip(t)
	run(t, "./canonicaldata.exe", "staticiface")
}

func TestCanonicalTypeAssertDescriptors(t *testing.T) {
	globalSkip(t)
	run(t, "./canonicaldata.exe", "assertdesc")
}

func TestCanonicalStaticDataAddrs(t *testing.T) {
	globalSkip(t)
	run(t, "./canonicaldata.exe", "dataaddr")
}

// TestCanonicalStaticFuncValue is the scan's negative pin: the one
// static-data word whose target is TEXT rather than data. A funcval
// container (pkg.Func·f, writable and therefore scannable under
// -buildmode=plugin) holds an R_ADDR to a host-shared function body,
// which peLoadImportLibrary leaves typed SWINDOWS/SHOSTOBJ — the same
// two types a host-shared DATA symbol carries. Routing it through the
// udaddr rewrite stores *(__imp_X), i.e. the PLT stub's instruction
// bytes, over an address the linker had already resolved correctly.
func TestCanonicalStaticFuncValue(t *testing.T) {
	globalSkip(t)
	run(t, "./canonicaldata.exe", "staticfunc")
}

// TestRejectChainedModule drives the stage-2 ABI rejection end to end
// and pins its residue handling (trendvidia/go#48). The host swaps the
// plugin file between the fence's two reads through package plugin's
// test hook, so plugin.Open maps a marker from a foreign generation
// whose DllMain chains a REAL moduledata; it asserts the stage-2
// error, the negative-cache "(previous failure)" answer, the resident
// mapping and the restored chain tail. It then links a PAGE_NOACCESS
// page as the rejected node and runs the same treatment, so any read
// of a foreign-layout residue faults the fixture. A healthy sibling
// must still load afterwards. The host prints skip on platforms
// without the Windows loader half.
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
// are successes or clean errors only and each goroutine sees the
// post-Close error state itself, sampled func values panic via the
// closed-plugin stub when called afterwards, and a second Close is an
// idempotent no-op. The fixture host AND plugin are built with
// raceFlags (not this harness, which only execs them), so where the
// platform has a race detector the pre-fix unlocked map read is
// reported as a data race and fails the run.
func TestLookupDuringClose(t *testing.T) {
	globalSkip(t)
	run(t, "./lookuprace.exe")
}

// TestOpenSymbolFailure drives open()'s could-not-find-symbol path
// twice (trendvidia/go#37). That mid-load failure must mark the
// cached *Plugin failed and close its loaded channel; it used to
// return with both unset, leaving a placeholder every later Open of
// the same path blocked on forever. The fixture asserts that every
// Open reports the error instead of hanging: the racers that were
// parked on <-p.loaded when the load failed, and the later serial
// Open, which gets it as "(previous failure)".
func TestOpenSymbolFailure(t *testing.T) {
	globalSkip(t)
	run(t, "./lookupfail.exe")
}

// TestDependentsDuringClose races HasDependents/Dependents against
// Close (trendvidia/go#36). Both accessors must snapshot
// p.pluginpath under pluginsMu AND copy it into host memory there,
// since closePlugin rewrites the field and then unmaps the bytes it
// pointed at, all under the same mutex. The fixture and its plugins
// are built -race where supported, so the pre-fix unlocked read
// fails this test as a reported DATA RACE; it keeps a second plugin
// mapped so a snapshot that still points into the closed plugin is
// dereferenced rather than skipped on a length mismatch.
func TestDependentsDuringClose(t *testing.T) {
	globalSkip(t)
	out := run(t, "./depsrace.exe")
	if !strings.Contains(out, "depsrace ok") {
		t.Fatalf("depsrace.exe did not run to completion:\n%s", out)
	}
}

// TestCloseZeroValue closes Plugin values that Open did not hand
// out (trendvidia/go#35). Close must return an error for both
// spellings, and must be able to format that error even when the
// value's pluginpath points into an image that is already unmapped: the zero value, whose empty pluginpath used to select
// the HOST module in the runtime, mark it bad and empty
// activeModules() — after which the next GC killed the process
// silently with exit status 0 — and a struct copy of an opened
// Plugin, which passes every field test and would unmap the image
// out from under Open's still-cached original. The fixture asserts
// both errors and that the original still works, then GCs, captures
// a stack and churns the heap, and must print "survived".
func TestCloseZeroValue(t *testing.T) {
	globalSkip(t)
	out := run(t, "./zeroclose.exe")
	if !strings.Contains(out, "survived") {
		t.Fatalf("zeroclose.exe did not report survival:\n%s", out)
	}
	// run() only fails on a non-zero exit, and the failure class
	// this fixture is about (a host module unlinked, or a fault
	// formatting a rejection) can end the process without one.
	// Require the last line too, not just the midpoint.
	if !strings.Contains(out, "copy rejected, original closed") {
		t.Fatalf("zeroclose.exe did not finish the struct-copy legs:\n%s", out)
	}
}

// TestLookupOnCopy pins the Lookup half of the struct-copy contract
// (trendvidia/go#64): Lookup accepts a copy of an Open-returned
// Plugin while the plugin is live, and returns an error — rather than
// hashing symName against a syms map whose keys live in the unmapped
// symbol table, or formatting a pluginpath header into unmapped
// RODATA — once the original has been closed. A fault ends the
// process without a clean exit code on every platform this runs on,
// but the last line is required as well so a silent death cannot
// pass as success.
func TestLookupOnCopy(t *testing.T) {
	globalSkip(t)
	out := run(t, "./lookupcopy.exe")
	if !strings.Contains(out, "lookup on copy: done") {
		t.Fatalf("lookupcopy.exe did not finish:\n%s", out)
	}
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

// TestGCAfterClose runs a full GC cycle, with the collector on the
// whole time, after a plugin has genuinely been unloaded. Every cycle
// starts with clearpools, which calls through runtime.poolcleanup —
// on Windows a host-shared global that the plugin's copy of package
// sync used to overwrite at Open with a funcval inside the plugin's
// image, so the first GC after Close faulted dereferencing unmapped
// memory (trendvidia/go#61). The fixture churns sync.Pool around the
// unload so the hook demonstrably still runs.
func TestGCAfterClose(t *testing.T) {
	globalSkip(t)
	out := run(t, "./gcafterclose.exe")
	if !strings.Contains(out, "survived GC after Close") {
		t.Fatalf("gcafterclose.exe did not report survival:\n%s", out)
	}
}

// runExpectFault runs bin, requiring the child to die from an
// unrecoverable runtime fault: the probe holds a reference class
// Plugin.Close does not protect, and using it after Close must crash
// the process ("fatal error: fault") rather than return. A clean exit
// fails, as does a death without the fault marker — in particular a
// recoverable "panic:" exit would mean the stub rewrite over-reached
// onto an unprotected class.
// runWithTimeout runs bin under a deadline and fails if it outlives it.
//
// For a test whose regression mode is a wedged process rather than a bad
// answer, an ordinary run gives no signal at all — it just never returns,
// and the package times out tens of minutes later naming no test.
func runWithTimeout(t *testing.T, d time.Duration, bin string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %v; output so far:\n%s", bin, d, out)
	}
	if err != nil {
		t.Fatalf("%s: %v\n%s", bin, err, out)
	}
}

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
//   - if the path is reopened instead and the loader maps the new
//     instance at the base the closed one occupied, stale references
//     silently alias the NEW instance.
//
// The two fatal classes each run in their own subprocess via
// runExpectFault. The aliasing cell runs as a subprocess too. It
// first pins that the reopen really produced a fresh instance (the
// reopened Counter reads its declared 0, not the 1 the closed
// instance was left at), because every claim below rests on that.
// The aliasing assertions themselves are then conditional: the
// loader does not promise to reuse the base (on Windows it was
// measured moving about 1 in 6 runs on arm64,
// trendvidia/go#47), and a stale pointer into a moved-away range is
// unmapped or aliases something unrelated, so the probe compares the
// two Lookup addresses first and only asserts aliasing when they
// match. Either outcome is a pass; the parent requires the probe to
// say which it took so a silently non-asserting fixture cannot hide.
// TestUpgradeStalePointer pins what a pointer held across an extension
// UPGRADE does (trendvidia/go#122). Upgrading is close(v1) then
// open(v2) — the flow unloading exists to serve — and the loader reuses
// a freed image's base despite ASLR, so a stale pointer used to address
// whatever the NEW image put at that offset. The fixture's v2 moves its
// fields deliberately, so the host writing what it believes is
// Threshold silently set Budget: a wrong value applied to live state,
// with no error anywhere.
//
// Every platform reserves the freed range on unload now, so the stale
// write faults: Windows via VirtualAlloc (trendvidia/go#122), POSIX via
// a hint-only mmap (#123). The fixture's own "corrupted Budget" branch
// is what this asserts against — reaching it means a reservation
// silently stopped happening and the upgrade path is quietly wrong
// again, which is the failure this whole fixture exists to catch.
// TestUpgradeSafe pins the contract Plugin.Close documents for
// replacing a loaded plugin: a host that reaches the plugin only
// through Lookup-obtained function values, and exchanges only types it
// already knows, upgrades cleanly with nothing to release first. The
// leftover handle on the old build panics recoverably with the
// documented value, and the new build is unaffected.
//
// It is the positive counterpart of TestUpgradeStalePointer. That one
// proves breaking the contract fails loudly; without this one, a change
// that made every upgrade fault would satisfy it and be useless.
func TestUpgradeSafe(t *testing.T) {
	globalSkip(t)
	out := run(t, "./upgradesafe.exe")
	if !strings.Contains(out, "upgradesafe ok") {
		t.Fatalf("upgradesafe did not report success:\n%s", out)
	}
}

func TestUpgradeStalePointer(t *testing.T) {
	globalSkip(t)
	runExpectFault(t, "./upgrade.exe")
}

func TestCloseHazardEnvelope(t *testing.T) {
	globalSkip(t)
	runExpectFault(t, "./hazards.exe", "closure")
	runExpectFault(t, "./hazards.exe", "dataptr")
	// The alias cell no longer tolerates a "skipped" outcome. It used
	// to, because whether the loader reused a freed base was its
	// decision and it varied (measured at about 1 in 6 on windows/arm64,
	// trendvidia/go#47). Unload now reserves the range (#122, #123), so
	// reusing the base is not a loader coin-flip any more — it is the
	// reservation having failed to happen, which is exactly what this
	// must catch.
	out := run(t, "./hazards.exe", "alias")
	if !strings.Contains(out, "hazards alias ok:") {
		t.Fatalf("hazards alias: want the reopened image at a different base; output:\n%s", out)
	}
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

// TestGoDomainModuleCanonicalData exercises cross-module data
// canonicalisation for packages hosted under a go.*-style module
// domain (trendvidia/go#108). isUserPackageName's old blanket `go.`
// prefix arm — meant for the go.typeptr./go.itabptr./go.funcptr. slot
// families — also matched module domains like go.uber.org, so such a
// package's globals were never registered in userDataExports/Imports
// and each module kept its own instance. The fixture's shared packages
// live under go.example.org/fakemod: lib.Counter is plugin-only
// (registry-canonicalised), hostlib.Value is host-linked
// (export-table-shared on Windows); the host asserts one instance of
// each.
func TestGoDomainModuleCanonicalData(t *testing.T) {
	globalSkip(t)
	run(t, "./godomain.exe")
}

// TestCrossModuleFuncValueIdentity pins that a func value means the
// same object in every module (trendvidia/go#106). A func value points
// at a one-word funcval, and each module emits its own `pkg.Fn·f`; when
// Fn's body is the host's, a plugin's copy holds the address of the
// plugin's IAT thunk. Calls worked through that thunk, so the symptom
// was silent: identity comparisons failed and runtime.FuncForPC named
// the value ".rel", the linker's thunk table, where POSIX names the
// function.
//
// The fixture covers all four shapes — host-linked var, materialized
// value, statically initialized word in a plugin-only package, and peer
// agreement — because a fix that repairs only the static word would
// pass a narrower test while leaving the materialized case broken.
func TestCrossModuleFuncValueIdentity(t *testing.T) {
	globalSkip(t)
	run(t, "./funcident.exe")
}

// TestRootModuleCanonicalData exercises the neighbouring arm of the
// same gate (trendvidia/go#110): a package at the ROOT of a dotted
// module path. objabi.PathToPrefix escapes every dot after the final
// slash, so when the whole import path is the last element the symbol
// name carries no slash — `module example.com` gives
// `example%2ecom.Counter` — and isUserPackageName's "no slash means
// stdlib top-level" arm rejected it, so the global never reached
// userDataExports/Imports and each module kept its own instance on
// Windows. rootcom.Counter (plugins only) covers the registry path and
// rootorg.Value (host-linked) the export-table path; the host asserts
// one instance of each.
func TestRootModuleCanonicalData(t *testing.T) {
	globalSkip(t)
	run(t, "./rootmod.exe")
}

// TestEscapedSymbolNameSharing pins that a plugin can bind a host-shared
// package whose symbol names carry a PathToPrefix %-escape
// (trendvidia/go#116). objabi.PathToPrefix escapes every '.' after the
// final slash, so a package whose import path ends in a dotted element
// — every gopkg.in path in the wild: yaml.v2, yaml.v3, check.v1 — has a
// `%2e` in all of its symbol names. On Windows those names become
// SDYNIMPORT when the plugin binds them from the host's import library,
// and initdynimport read the first '%' as a stdcall `%n` argument-count
// decoration: the link failed with "failed to parse stdcall decoration"
// after truncating `gopkg.in/yaml%2ev2.Set` to `gopkg.in/yaml`. The
// build of this fixture is most of the test; the host then asserts the
// global really is one instance.
// TestSlashlessNameCanonicalData pins both halves of the gate that
// isUserPackageName's "no slash means stdlib" arm used to close
// (trendvidia/go#115): a package at the root of a module named without
// a domain — `go mod init extensions`, which is how first-party
// extension code is laid out — and a slashless STDLIB package that the
// plugins link and the host does not, where the host's import library
// cannot share it either.
//
// Nothing separates `extensions.Counter` from `os.Args` by name, and
// the linker has no std-package list to ask instead, so the arm was a
// guess standing in for a fact. POSIX gives one instance in both cases;
// this is how Windows is held to that.
func TestSlashlessNameCanonicalData(t *testing.T) {
	globalSkip(t)
	run(t, "./dotless.exe")
}

func TestEscapedSymbolNameSharing(t *testing.T) {
	globalSkip(t)
	run(t, "./escname.exe")
}

// TestPluginLocalAsmCall pins that a plugin whose OWN text calls ABI0
// assembly loads and runs (trendvidia/go#117). On windows/amd64 the
// compiler rewrites the g-register reload after such a call into a load
// through a symbol named `__imp_runtime.tls_g`; internal linking used to
// wrap that into a slot holding the image's rebasing delta, so the
// reload faulted with 0xc0000005 inside plugin.Open — before any Lookup
// could guard against it, since edwards25519's init makes the call.
//
// Every other fixture here shares its assembly-carrying packages with
// the host, so the call happens in host text, which is not -dynlink and
// uses the direct two-instruction form. This one's host links no
// crypto, which is the whole point of it.
// TestPEImpWrapperTargetsIATSlot in cmd/link/internal/ld pins the slot's
// content directly, and can fail on any platform; this one is the
// does-the-process-survive half.
func TestPluginLocalAsmCall(t *testing.T) {
	globalSkip(t)
	run(t, "./asmplugin.exe")
}

// TestInternalPackageInitData pins that a package whose `..inittask` is
// host-shared has its DATA host-shared too (trendvidia/go#74). The
// host links internal/poll through os but not net, so internal/poll's
// init ran host-side and the plugin dropped it from its schedule, while
// net's init stays plugin-local and runs at Open — and net.init.0 calls
// internal/poll.InitWSA, a sync.OnceFunc value that init assigns. With
// the package's data left plugin-local the plugin read nil and died in
// plugin.Open on windows/{amd64,arm64}.
//
// TestPEInittaskSharesWithItsData in cmd/link/internal/ld pins the gate
// itself and needs no Windows host; this is the does-the-process-
// survive half.
func TestInternalPackageInitData(t *testing.T) {
	globalSkip(t)
	run(t, "./internalinit.exe")
}

func TestUniqueTypesAndItabs(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "iface.exe", "./iface")
	goCmdBuildPluginFor(t, "iface", "./iface_a")
	goCmdBuildPluginFor(t, "iface", "./iface_b")
	run(t, "./iface.exe")
}

// TestTypetabLinkedTables pins what the linker does with the -typetab
// slots that TestTypetabSlotsPresent (cmd/compile/internal/test) only
// proves the compiler emits. ld/typelink.go builds runtime.typeptrs /
// runtime.typeptroffs / runtime.itabptrs from a reachable-symbol walk
// with three silent `continue` filters (size != ptrsize, no reloc,
// duplicate name); a wrong filter yields an EMPTY table and every
// cross-module type dedup quietly stops happening. And each slot must
// land in writable data: under -buildmode=plugin the linker's
// makeRelroForSharedLib promotes read-only kinds carrying relocations
// into RELRO, which the runtime's typeptrsinit / itabptrsinit rewrite
// would then fault on. Both static properties are read out of the
// linked ELF images, and RELRO promotion is an ELF mechanism in the
// first place (Mach-O has no RELRO, PE no PT_GNU_RELRO), so
// checkTypetabTables runs on linux only -- the one ELF platform the
// plugin matrix runs; freebsd/android would qualify but nothing here
// exercises them, so the gate names the measured platform rather than
// the mechanism.
//
// Every platform still builds the -typetab host and plugins and runs
// the host, but what that run can observe differs by platform, and
// the difference is why the static half is not redundant. On windows
// the slots ARE the dedup mechanism -- the PE loader interposes
// nothing, and cmd/go forces -typetab onto every windows build
// (work/init.go; ROADMAP.md P1) -- so an empty table fails iface.exe's
// pointer-identity assertions outright. On POSIX the dynamic loader
// binds the plugin's shared type descriptors to the host's copies
// regardless, so the run passes with or without populated tables and
// only checkTypetabTables observes them. Do not drop the linux static
// half on the theory that running the host covers it everywhere.
//
// The fixture is iface_a + iface_b under the iface host: both build
// a non-empty interface value from a type shared with the host, so a
// -typetab build produces at least one go.typeptr.* and one
// go.itabptr.* slot per plugin, and running the host proves the
// tables the linker wrote are ones the runtime accepts.
func TestTypetabLinkedTables(t *testing.T) {
	globalSkip(t)

	// Own directory: TestUniqueTypesAndItabs builds its own iface_a
	// and iface_b plugins in the module root, and its host opens them
	// by bare name from there, so this test's build of the same
	// fixtures must not overwrite them.
	dir := "typetab"
	if err := os.MkdirAll(dir, 0777); err != nil {
		t.Fatal(err)
	}
	// Private build cache. This fork ships a release-style VERSION, so
	// cmd/go's toolID for the compiler and linker is version-only and
	// does NOT change when they are rebuilt from source (trendvidia/
	// go#81). A shared GOCACHE therefore hands this test std archives
	// linked by whatever toolchain filled the cache, and the test
	// silently measures that one instead: measured 2026-08-27 on
	// linux/arm64, a clean HEAD toolchain reporting 40+ bogus "promoted
	// into RELRO" failures for runtime.* slots left behind by an
	// earlier experiment's compiler. A false PASS is available the same
	// way. Everything below is built with -typetab anyway, so a private
	// cache costs one std build rather than reusing anything real.
	t.Setenv("GOCACHE", t.TempDir())
	// go build's own -gcflags (no pattern) applies only to the named
	// package; -typetab must reach every package linked into the plugin
	// so that types shared with the host are all routed through slots.
	// all= would override the harness's GO_GCFLAGS, so carry them along.
	allGcflags := "-gcflags=all=" + strings.TrimSpace(gcflags+" -typetab")
	goCmd(t, "build", allGcflags, "-o", filepath.Join(dir, "iface.exe"), "./iface")
	// Not goCmdBuildPluginFor("iface", ...): that helper resolves the
	// import library next to hostImplib, i.e. to the module root's
	// iface.exe.a, left there by TestUniqueTypesAndItabs' host build.
	// That is a different binary -- different gcflags, and built from
	// the shared GOCACHE rather than the private one set above -- and
	// it is not the host this test runs; under -run
	// TestTypetabLinkedTables it does not exist at all. (The difference
	// is not -typetab: cmd/go forces -typetab onto every windows build,
	// work/init.go.) These
	// plugins must link against the -typetab host built just above,
	// whose import library the linker wrote alongside it. Absolute
	// path: the linker does not run in this test's cwd -- cmd/go
	// absolutises -pluginimplib before it chdirs (work/gc.go), but say
	// it here rather than depend on that.
	implib := ""
	if runtime.GOOS == "windows" {
		var err error
		if implib, err = filepath.Abs(filepath.Join(dir, "iface.exe.a")); err != nil {
			t.Fatal(err)
		}
	}
	goCmdBuildPluginWithImplib(t, implib, allGcflags, "-o", filepath.Join(dir, "iface_a"+pluginSuffix), "./iface_a")
	goCmdBuildPluginWithImplib(t, implib, allGcflags, "-o", filepath.Join(dir, "iface_b"+pluginSuffix), "./iface_b")

	// Static half: linux only, see the doc comment.
	if runtime.GOOS == "linux" {
		for _, so := range []string{"iface_a" + pluginSuffix, "iface_b" + pluginSuffix} {
			t.Run(so, func(t *testing.T) {
				checkTypetabTables(t, filepath.Join(dir, so))
			})
		}
	}

	// testenv.Command, not exec.Command: this host dlopens two plugins,
	// and a wedged module load would otherwise hang to the package
	// timeout with no stack dump. testenv gives it the test's deadline
	// and SIGQUITs it.
	cmd := testenv.Command(t, "./iface.exe")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host built with -typetab could not load -typetab plugins: %v\n%s", err, out)
	}
}

// checkTypetabTables asserts, on a linked ELF plugin, that the linker
// produced non-empty, pointer-aligned typeptr/itabptr tables whose entry
// counts match the slot symbols it linked, and that every slot lives in
// writable, non-RELRO data. It reads the ELF symbol table rather than
// `go tool nm` codes: nm reports 'D' for any SHF_WRITE section, and
// .data.rel.ro is SHF_WRITE too (the loader applies relocations before
// mprotecting it), so a RELRO slot would read as 'D' and pass.
func checkTypetabTables(t *testing.T, image string) {
	t.Helper()
	f, err := elf.Open(image)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", image, err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("%s: no symbol table: %v", image, err)
	}
	ptrsize := uint64(4)
	if f.Class == elf.ELFCLASS64 {
		ptrsize = 8
	}

	var relro []*elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_GNU_RELRO {
			relro = append(relro, p)
		}
	}
	// Assert the segment exists rather than letting the containment
	// loop below iterate zero times: a plugin is linked with relro
	// (ld.UseRelro is true for -buildmode=plugin on ELF, and an
	// external link of a shared object emits PT_GNU_RELRO unless
	// something passed -z norelro), so no segment means the RELRO half
	// of this test silently measured nothing and reported PASS.
	if len(relro) == 0 {
		t.Errorf("%s: no PT_GNU_RELRO segment; the RELRO containment check below would observe nothing (linked with -z norelro?)", image)
	}

	bySym := make(map[string]elf.Symbol)
	slotCount := map[string]int{"go.typeptr.": 0, "go.itabptr.": 0}
	for _, s := range syms {
		if s.Section == elf.SHN_UNDEF {
			continue
		}
		if strings.HasPrefix(s.Name, "runtime.typeptr") || strings.HasPrefix(s.Name, "runtime.itabptr") {
			bySym[s.Name] = s
			continue
		}
		var prefix string
		for p := range slotCount {
			if strings.HasPrefix(s.Name, p) {
				prefix = p
			}
		}
		if prefix == "" {
			continue
		}
		slotCount[prefix]++
		if s.Size != ptrsize {
			t.Errorf("%s: slot %s has size %d, want %d; typelink's size filter would drop it", image, s.Name, s.Size, ptrsize)
		}
		if int(s.Section) >= len(f.Sections) {
			t.Errorf("%s: slot %s has section index %d out of range", image, s.Name, s.Section)
			continue
		}
		sect := f.Sections[s.Section]
		if sect.Flags&elf.SHF_WRITE == 0 || sect.Flags&elf.SHF_EXECINSTR != 0 {
			t.Errorf("%s: slot %s is in %s (flags %v), want a writable data section", image, s.Name, sect.Name, sect.Flags)
		}
		if strings.HasPrefix(sect.Name, ".data.rel.ro") {
			t.Errorf("%s: slot %s was promoted into RELRO section %s; the runtime rewrites slots at module load and would fault", image, s.Name, sect.Name)
		}
		for _, p := range relro {
			if s.Value >= p.Vaddr && s.Value < p.Vaddr+p.Memsz {
				t.Errorf("%s: slot %s at %#x lies inside PT_GNU_RELRO [%#x,%#x)", image, s.Name, s.Value, p.Vaddr, p.Vaddr+p.Memsz)
			}
		}
	}

	table := func(name string, entry uint64) (uint64, bool) {
		s, ok := bySym[name]
		if !ok {
			t.Errorf("%s: no %s symbol in the symbol table", image, name)
			return 0, false
		}
		if s.Size == 0 {
			t.Errorf("%s: %s is empty; typelink's reachable-symbol walk found no slots", image, name)
		}
		if s.Size%entry != 0 {
			t.Errorf("%s: %s has size %d, not a multiple of the %d-byte entry", image, name, s.Size, entry)
		}
		return s.Size / entry, true
	}
	ntypeptrs, okTypeptrs := table("runtime.typeptrs", ptrsize)
	ntypeptroffs, okTypeptroffs := table("runtime.typeptroffs", 4)
	nitabptrs, okItabptrs := table("runtime.itabptrs", ptrsize)
	// Nothing else catches a short typeptroffs. The runtime does check
	// len(md.typeptrs) != len(md.typeptroffs) (runtime/type.go,
	// typeptrsinit), but that throw cannot fire: ld/symtab.go publishes
	// BOTH slice headers with the count it derived from runtime.typeptrs
	// alone, never reading typeptroffs' own size. A typeptroffs shorter
	// than its symbol claims would leave typeptrsinit indexing past the
	// symbol into adjacent .noptrdata and canonicalising slots to
	// garbage typeOffs, silently. So compare the symbol sizes here.
	if okTypeptrs && okTypeptroffs && ntypeptroffs != ntypeptrs {
		t.Errorf("%s: runtime.typeptroffs has %d entries, runtime.typeptrs has %d; typeptrsinit would index typeptroffs out of bounds", image, ntypeptroffs, ntypeptrs)
	}
	// Every slot the linker kept reachable got exactly one table entry:
	// the walk's filters must not drop a slot, and DUPOK dedup must not
	// double-count one. Exact equality holds because this test is ELF-
	// only: on a Windows plugin ld/typelink.go's staticCanonWords
	// appends static-data word entries to both tables after the slots,
	// and the counts would then differ by design.
	// Gated on the symbol existing: table() already reported a missing
	// one, and a second "N slots linked but the table has 0 entries"
	// would point the reader at the linker's dedup instead.
	if got := uint64(slotCount["go.typeptr."]); okTypeptrs && got != ntypeptrs {
		t.Errorf("%s: %d go.typeptr.* slots linked but runtime.typeptrs has %d entries", image, got, ntypeptrs)
	}
	if got := uint64(slotCount["go.itabptr."]); okItabptrs && got != nitabptrs {
		t.Errorf("%s: %d go.itabptr.* slots linked but runtime.itabptrs has %d entries", image, got, nitabptrs)
	}
	if slotCount["go.typeptr."] == 0 || slotCount["go.itabptr."] == 0 {
		t.Errorf("%s: linked %d go.typeptr.* and %d go.itabptr.* slots; the fixture should produce both, so -typetab did not reach the compile", image, slotCount["go.typeptr."], slotCount["go.itabptr."])
	}
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

// TestCgoPluginNotifyExport pins the cgo runtime-init handshake
// (trendvidia/go#62, trendvidia/go#130).
//
// A plugin containing runtime/cgo carries its own libcgo, whose
// runtime_init_done starts clear. Every C->Go entry point cgo emits
// blocks on it until plugin.Open calls x_cgo_notify_runtime_init_done
// through the DLL's export table, so a plugin missing that export takes
// its first callback and never returns.
//
// Both link modes were broken, in different ways. Externally linked, the
// export existed only because mingw ld auto-exports what it can, so
// narrowing the export set removed it (2195 exported names became 6, #62).
// Internally linked, it was never exported at all and the notify was
// never called: measured on windows/arm64, suppressing the call leaves
// runtime_init_done at 0 after Open and the callback hangs, where calling
// it leaves the flag at 1 and the callback returns (#130).
//
// So the default build is the case that matters on every platform, and
// the narrowed build is the extra one #62 was about.
func TestCgoPluginNotifyExport(t *testing.T) {
	globalSkip(t)
	if runtime.GOOS != "windows" {
		t.Skip("the cgo runtime-init handshake this exercises is Windows-only")
	}
	if !testenv.HasCGO() {
		t.Skip("needs cgo: the plugin must contain runtime/cgo")
	}
	goCmd(t, "build", "-o", "cgonotify.exe", "./cgonotify/host")
	implib := filepath.Join(filepath.Dir(hostImplib), "cgonotify.exe.a")

	// A regression HANGS rather than failing -- the callback blocks in
	// _cgo_wait_runtime_init_done with nothing logged -- so every run
	// here is under a deadline.
	t.Run("default", func(t *testing.T) {
		goCmdBuildPluginFor(t, "cgonotify",
			"-o=cgonotify-plugin.dll", "./cgonotify/plug")
		runWithTimeout(t, 2*time.Minute, "./cgonotify.exe")
	})

	// A plugin whose only cgo is `import "C"`. runtime/cgo is in the
	// link, so the linker's iscgo is set, but nothing references the
	// handshake and deadcode drops it -- the image has neither the wait
	// to block on nor the notify to publish. Exporting one anyway is
	// what broke six tests on windows/arm64: on the internal-link path
	// cmd/link writes the export table itself, an unreachable symbol has
	// no address yet, and the export went out as RVA 0xc00003e8, so
	// plugin.Open called base+0x3e8 and died inside the PE headers
	// (trendvidia/go#130).
	t.Run("import C only", func(t *testing.T) {
		goCmd(t, "build", "-o", "cgonotify-bare.exe", "./cgonotify/barehost")
		goCmdBuildPluginFor(t, "cgonotify-bare",
			"-o=cgonotify-bare.dll", "./cgonotify/bare")
		runWithTimeout(t, 2*time.Minute, "./cgonotify-bare.exe")
	})

	// The external-link path, where the export used to be mingw's
	// accident and -Wl,--exclude-all-symbols swept it away with
	// everything else the .def did not name. Ask the platform for the
	// link rather than assuming which ones can do it: windows/arm64
	// cannot link a cgo plugin externally at all (#131), and if that
	// changes this starts covering it instead of silently not.
	t.Run("exports narrowed", func(t *testing.T) {
		ldflags := "-ldflags=-linkmode=external -extldflags=-Wl,--exclude-all-symbols" +
			" -pluginimplib=" + implib
		build := exec.Command(filepath.Join(goroot, "bin", "go"), "build", "-gcflags", gcflags,
			"-buildmode=plugin", ldflags, "-o=cgonotify-plugin.dll", "./cgonotify/plug")
		if out, err := build.CombinedOutput(); err != nil {
			t.Skipf("cannot link a cgo plugin externally here, so there is no auto-export to lose: %v\n%s", err, out)
		}
		runWithTimeout(t, 2*time.Minute, "./cgonotify.exe")
	})
}

// TestHotSwapUpgrade pins upgrading a plugin in place under live
// traffic: load a strategy, run calls through it from several
// goroutines, replace it with a newer build, and repeat, without
// restarting the process.
//
// Two facts, and the second is the one that shapes any supervisor built
// on this:
//
//   - close-before-load works, repeatedly, with no call ever landing in
//     a retired generation;
//   - load-before-close does NOT. runtime.recordPkgDependencies makes
//     the first module carrying a (package, link-time hash) its owner,
//     and later modules carrying that package depend on the owner. Two
//     builds of a plugin share their packages, so the NEWER generation
//     depends on the OLDER one and Close(older) is refused. The
//     dependency always points newer->older, so only the newest
//     generation is ever closable — the opposite of what an upgrade
//     needs, and the reason the order is forced.
//
// Measured while writing this (200 cycles, 4 workers): the swap window
// where no plugin is serving is 54ms p50 on windows/amd64, 26ms on
// windows/arm64, 141ms on darwin/arm64, and each cycle costs ~0.5 MiB of
// reserved address space. testdata/hotswap/host reports those numbers
// when run directly; this test only holds the behaviour.
func TestHotSwapUpgrade(t *testing.T) {
	globalSkip(t)
	goCmd(t, "build", "-o", "hotswap.exe", "./hotswap/host")
	goCmdBuildPluginFor(t, "hotswap", "-o=strat_v1"+pluginSuffix, "./hotswap/strat_v1")
	goCmdBuildPluginFor(t, "hotswap", "-o=strat_v2"+pluginSuffix, "./hotswap/strat_v2")

	args := []string{"-v1=strat_v1" + pluginSuffix, "-v2=strat_v2" + pluginSuffix}

	// The constraint. If this stops holding, the supervisor pattern in
	// the fixture is no longer the only one available -- which would be
	// good news, and should be read rather than silently absorbed.
	t.Run("older generation is pinned by newer", func(t *testing.T) {
		out := run(t, "./hotswap.exe", append(args, "-probe-dependency")...)
		if !strings.Contains(out, "HasDependents=true") {
			t.Errorf("probe reported no dependents; want the newer generation to pin the older\n%s", out)
		}
		if !strings.Contains(out, "has dependents") {
			t.Errorf("Close(older) was not refused; the upgrade order may no longer be forced\n%s", out)
		}
	})

	// The pattern that works. A regression here can HANG (a drain that
	// never completes) rather than fail, so it runs under a deadline.
	t.Run("close before load, under traffic", func(t *testing.T) {
		runWithTimeout(t, 3*time.Minute, "./hotswap.exe",
			append(args, "-cycles=12", "-workers=4", "-settle=5ms")...)
	})
}
