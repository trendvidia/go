// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package plugin_test

import (
	"bytes"
	"cmd/cgo/internal/cgotest"
	"context"
	"flag"
	"fmt"
	"internal/platform"
	"internal/testenv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	goCmd(nil, "build", "-o", "closeunmaps.exe", "./closeunmaps")
	goCmdBuildPluginFor(nil, "closeunmaps", "-o=closeunmaps-plugin1"+pluginSuffix, "./plugin1")

	goCmd(nil, "build", "-o", "refcount.exe", "./refcount/host")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugA"+pluginSuffix, "./refcount/plugA")
	goCmdBuildPluginFor(nil, "refcount", "-o=plugB"+pluginSuffix, "./refcount/plugB")

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
