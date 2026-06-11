// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package plugin_test exercises -buildmode=plugin without requiring
// cgo on Windows. The companion test tree at
// cmd/cgo/internal/testplugin covers the cgo paths and is gated on
// CGO_ENABLED=1. This package's fixtures contain no `import "C"`
// and so build under CGO_ENABLED=0 on Windows; on POSIX, plugin
// support still requires cgo (libdl-based dlopen wrapper) and the
// test skips when cgo is unavailable.
package plugin_test

import (
	"bytes"
	"flag"
	"fmt"
	"internal/platform"
	"internal/testenv"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// pluginSuffix is the platform's plugin shared-library extension.
var pluginSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

// globalSkip is invoked by every test to skip uniformly when the
// platform doesn't support plugins, or when POSIX is missing cgo.
var globalSkip = func(t *testing.T) {}

// modRoot is the GOPATH/src/testplugin directory used for the
// canonical-version host and plugin builds.
var modRoot string

// altRoot is the alt-GOPATH/src/testplugin used to build the
// mismatch plugin against a different common package.
var altRoot string

// hostImplib is the path to host.exe.a (Windows import library).
// Empty on non-Windows where it's unused.
var hostImplib string

// mismatchHostImplib is the import library for mismatchhost.exe.
// Empty on non-Windows.
var mismatchHostImplib string

func TestMain(m *testing.M) {
	flag.Parse()
	log.SetFlags(log.Lshortfile)
	os.Exit(testMain(m))
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
	// On POSIX, the plugin runtime is implemented with cgo+libdl
	// (plugin/plugin_dlopen.go). Without cgo, plugin.Open returns
	// "plugin: not implemented". Windows uses LoadLibrary directly
	// and works without cgo — that's what this test exists to
	// cover.
	if !testenv.HasCGO() && runtime.GOOS != "windows" {
		globalSkip = func(t *testing.T) { t.Skip("plugin requires cgo on POSIX") }
		return m.Run()
	}

	GOPATH, err := os.MkdirTemp("", "cmd_internal_testplugin")
	if err != nil {
		log.Panic(err)
	}
	if os.Getenv("KEEP_TMPDIR") == "" {
		defer os.RemoveAll(GOPATH)
	}

	modRoot = filepath.Join(GOPATH, "src", "testplugin")
	altRoot = filepath.Join(GOPATH, "alt", "src", "testplugin")
	for srcRoot, dstRoot := range map[string]string{
		"testdata":                           modRoot,
		filepath.Join("testdata", "altpath"): altRoot,
	} {
		if err := overlayDir(dstRoot, srcRoot); err != nil {
			log.Panic(err)
		}
		if err := os.WriteFile(filepath.Join(dstRoot, "go.mod"), []byte("module testplugin\n"), 0666); err != nil {
			log.Panic(err)
		}
	}

	// Build canonical host first. On Windows, this emits
	// host.exe.a (the import library plugins link against).
	os.Setenv("GOPATH", GOPATH)
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	}
	os.Setenv("PWD", modRoot)
	os.Setenv("LD_LIBRARY_PATH", modRoot)

	if err := goCmd(nil, "", "build", "-o", "host.exe", "./host"); err != nil {
		log.Panic(err)
	}
	if runtime.GOOS == "windows" {
		hostImplib = filepath.Join(modRoot, "host.exe.a")
	}

	if err := goCmd(nil, "", "build", "-o", "mismatchhost.exe", "./mismatchhost"); err != nil {
		log.Panic(err)
	}
	if runtime.GOOS == "windows" {
		mismatchHostImplib = filepath.Join(modRoot, "mismatchhost.exe.a")
	}

	// Plugins built against the canonical common (linked against
	// host.exe.a on Windows).
	if err := goCmdBuildPlugin(nil, hostImplib, "-o", "p1"+pluginSuffix, "./p1"); err != nil {
		log.Panic(err)
	}
	if err := goCmdBuildPlugin(nil, hostImplib, "-o", "p2"+pluginSuffix, "./p2"); err != nil {
		log.Panic(err)
	}

	// Mismatch plugin: built from the altpath GOPATH so the
	// common it links against has different pkghash, then dropped
	// into modRoot so mismatchhost can Open it.
	os.Setenv("GOPATH", filepath.Join(GOPATH, "alt"))
	if err := os.Chdir(altRoot); err != nil {
		log.Panic(err)
	}
	os.Setenv("PWD", altRoot)
	if err := goCmdBuildPlugin(nil, mismatchHostImplib, "-o", filepath.Join(modRoot, "p-mismatch"+pluginSuffix), "./p1"); err != nil {
		log.Panic(err)
	}

	os.Setenv("GOPATH", GOPATH)
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	}
	os.Setenv("PWD", modRoot)

	return m.Run()
}

// goCmd runs `go op args...` with the provided extra env (each
// "K=V") layered on top of the inherited environment. Returns nil
// on success or a non-nil error containing combined stderr.
func goCmd(t *testing.T, env string, op string, args ...string) error {
	if t != nil {
		t.Helper()
	}
	full := append([]string{op}, args...)
	cmd := exec.Command(testenv.GoToolPath(nil), full...)
	if env != "" {
		cmd.Env = append(os.Environ(), env)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v\n%s", testenv.GoToolPath(nil), strings.Join(full, " "), err, stderr.String())
	}
	return nil
}

// goCmdBuildPlugin wraps `go build -buildmode=plugin`. On Windows
// it merges -pluginimplib=<implib> into -ldflags so the plugin's
// IAT points at the matching host's import library.
func goCmdBuildPlugin(t *testing.T, implib string, args ...string) error {
	if t != nil {
		t.Helper()
	}
	full := []string{"-buildmode=plugin"}
	if runtime.GOOS == "windows" && implib != "" {
		full = append(full, "-ldflags=-pluginimplib="+implib)
	}
	full = append(full, args...)
	return goCmd(t, "", "build", full...)
}

func runHost(t *testing.T, exe string) {
	t.Helper()
	cmd := exec.Command("./" + exe)
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("./%s: %v\n%s", exe, err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != "PASS" {
		t.Fatalf("./%s output:\n%s", exe, got)
	}
}

// TestBasic exercises plugin.Open + plugin.Lookup of a function and
// an exported variable, and confirms that two plugins share the
// host's single moduledata for `common` (cross-plugin package
// dedup).
func TestBasic(t *testing.T) {
	globalSkip(t)
	runHost(t, "host.exe")
}

// TestMismatch exercises the pkghashes mismatch guard (Bug A): a
// plugin compiled against a different version of `common` must be
// rejected by plugin.Open with "different version" in the error.
func TestMismatch(t *testing.T) {
	globalSkip(t)
	runHost(t, "mismatchhost.exe")
}

// stressPluginSrc is the template for the generated stress plugin
// variants. Each variant gets a distinct package directory (and so a
// distinct pluginpath) and a distinct V; all of them funnel Adds into
// the host-owned testplugin/stressshared counter.
const stressPluginSrc = `// Code generated by TestStress in plugin_test.go. DO NOT EDIT.
package main

import "testplugin/stressshared"

// V identifies the plugin variant; the host verifies it via Lookup.
var V int = %d

// Add bumps the shared counter owned by the host and returns the
// new total.
func Add(n int64) int64 { return stressshared.Add(n) }

func main() {}
`

// TestStress exercises the multi-plugin lifecycle under concurrency
// (ROADMAP A2, https://github.com/trendvidia/go/issues/3): rounds of
// parallel Opens (including duplicate Opens of already-loaded paths,
// which must dedup), concurrent calls through looked-up functions
// into a shared package, Close of a random subset, GC churn, reopen.
// The host imports testplugin/stressshared so its instance is
// canonical — Close is always permitted and the shared counter must
// exactly equal the sum of all Adds across all plugin generations.
func TestStress(t *testing.T) {
	globalSkip(t)

	// Close→reopen churn across rounds currently breaks on every
	// platform (SIGSEGV in dlopen on darwin/arm64, "plugin already
	// loaded" on linux/amd64, AV in addmoduledata on windows/arm64).
	// Single-cycle hot reload (TestCloseHotReload) works; multi-
	// plugin churn does not. Unskip when the lifecycle teardown is
	// fixed.
	t.Skip("multi-plugin close/reopen churn broken; see https://github.com/trendvidia/go/issues/5")

	const nPlugins = 8

	// Generate the plugin variants into the GOPATH copy of the module.
	for i := 0; i < nPlugins; i++ {
		dir := filepath.Join(modRoot, fmt.Sprintf("stressp%d", i))
		if err := os.MkdirAll(dir, 0777); err != nil {
			t.Fatal(err)
		}
		src := fmt.Sprintf(stressPluginSrc, i)
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0666); err != nil {
			t.Fatal(err)
		}
	}

	// Build the host first: on Windows this emits stresshost.exe.a,
	// the import library every variant links against.
	if err := goCmd(t, "", "build", "-o", "stresshost.exe", "./stresshost"); err != nil {
		t.Fatal(err)
	}
	implib := ""
	if runtime.GOOS == "windows" {
		implib = filepath.Join(modRoot, "stresshost.exe.a")
	}

	// Build the variants concurrently (bounded), mirroring how a real
	// build farm would produce them.
	var wg sync.WaitGroup
	errs := make(chan error, nPlugins)
	sem := make(chan struct{}, 4)
	for i := 0; i < nPlugins; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			name := fmt.Sprintf("stressp%d", i)
			if err := goCmdBuildPlugin(nil, implib, "-o", name+pluginSuffix, "./"+name); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	cmd := exec.Command("./stresshost.exe",
		"-plugins", fmt.Sprint(nPlugins), "-rounds", "5", "-hammers", "10", "-seed", "1")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("./stresshost.exe: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "PASS" {
		t.Fatalf("./stresshost.exe output:\n%s", got)
	}
}

// overlayDir copies srcRoot to dstRoot (symlinks where supported,
// byte copy otherwise). Avoids depending on cmd/cgo/internal/cgotest
// so this package has no transitive cgo-only deps.
func overlayDir(dstRoot, srcRoot string) error {
	dstRoot = filepath.Clean(dstRoot)
	if err := os.MkdirAll(dstRoot, 0777); err != nil {
		return err
	}
	srcRoot, err := filepath.Abs(srcRoot)
	if err != nil {
		return err
	}
	return filepath.Walk(srcRoot, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil || srcPath == srcRoot {
			return err
		}
		suffix := strings.TrimPrefix(srcPath, srcRoot)
		for len(suffix) > 0 && suffix[0] == filepath.Separator {
			suffix = suffix[1:]
		}
		dstPath := filepath.Join(dstRoot, suffix)
		perm := info.Mode() & os.ModePerm
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(srcPath)
			if err != nil {
				return err
			}
			perm = info.Mode() & os.ModePerm
		}
		if info.IsDir() {
			return os.MkdirAll(dstPath, perm|0200)
		}
		if err := os.Symlink(srcPath, dstPath); err == nil {
			return nil
		}
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		return err
	})
}
