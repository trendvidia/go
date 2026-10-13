// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"cmd/internal/objabi"
	"cmd/internal/sys"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"internal/buildcfg"
	"internal/testenv"
)

// newExportCandidates returns a windows/amd64 plugin-build context whose
// loader holds n distinct reachable CgoExportDynamic symbols — the inputs
// initdynexport's export-set walk selects on — plus one unreachable,
// export-marked symbol to confirm the walk filters it out.
func newExportCandidates(t *testing.T, buildmode string, n int) *Link {
	t.Helper()
	// linknew requires buildcfg.GOARCH to match the target arch; adopt
	// data_test.go's save-and-set so the fixture stays windows/amd64 on
	// any host.
	origGOARCH := buildcfg.GOARCH
	buildcfg.GOARCH = sys.ArchAMD64.Name
	t.Cleanup(func() { buildcfg.GOARCH = origGOARCH })
	ctxt := setUpContext(sys.ArchAMD64, false, objabi.Hwindows, buildmode, "internal")
	ldr := ctxt.loader
	for i := 0; i < n; i++ {
		sb := ldr.CreateSymForUpdate(fmt.Sprintf("p.F%d", i), 0)
		sb.SetType(sym.STEXT)
		sb.SetReachable(true)
		ldr.SetAttrCgoExportDynamic(sb.Sym(), true)
	}
	dead := ldr.CreateSymForUpdate("p.dead", 0)
	dead.SetType(sym.STEXT)
	// Created symbols are reachable by default; make this one dead so it
	// exercises the walk's reachability filter.
	dead.SetReachable(false)
	ldr.SetAttrCgoExportDynamic(dead.Sym(), true)
	return ctxt
}

// resetExportState points *flagOutfile at a throwaway name, clears
// -pluginexename and empties the dexport global around a test, restoring
// all three afterwards.
func resetExportState(t *testing.T) {
	t.Helper()
	oldOutfile := *flagOutfile
	*flagOutfile = filepath.Join(t.TempDir(), "test.dll")
	oldExeName := *flagPluginExeName
	*flagPluginExeName = ""
	oldDexport := dexport
	dexport = nil
	t.Cleanup(func() {
		*flagOutfile = oldOutfile
		*flagPluginExeName = oldExeName
		dexport = oldDexport
	})
}

// captureStderr runs f with os.Stderr redirected into a pipe and returns
// everything written during it. Tests calling it must not be parallel.
//
// The restore is deferred and the pipe is drained concurrently: f pokes
// linker globals, so a panic inside it must not leave os.Stderr pointing
// at a pipe nobody reads — that would swallow the panic trace and every
// later test's output — and output longer than the pipe buffer must not
// deadlock waiting for a reader that only starts after f returns.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := make(chan []byte, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	old := os.Stderr
	os.Stderr = w
	func() {
		// Restore on the way out however f leaves: a panic here
		// propagates with its trace going to the real stderr.
		defer func() {
			os.Stderr = old
			w.Close()
		}()
		f()
	}()
	return string(<-done)
}

// TestInitDynexportUnderLimit checks that a candidate set under the PE
// export limit is exported in full, unreachable symbols excluded, with no
// diagnostic.
func TestInitDynexportUnderLimit(t *testing.T) {
	ctxt := newExportCandidates(t, "plugin", 3)
	resetExportState(t)
	var got []loader.Sym
	diag := captureStderr(t, func() {
		initdynexport(ctxt)
		got = dexport
	})
	if len(got) != 3 {
		t.Fatalf("got %d exports, want 3", len(got))
	}
	if diag != "" {
		t.Errorf("unexpected diagnostics:\n%s", diag)
	}
}

// TestInitDynexportAtLimit pins the boundary: exactly peExportLimit
// candidates all fit, no warning, no truncation.
func TestInitDynexportAtLimit(t *testing.T) {
	ctxt := newExportCandidates(t, "plugin", peExportLimit)
	resetExportState(t)
	var got []loader.Sym
	diag := captureStderr(t, func() {
		initdynexport(ctxt)
		got = dexport
	})
	if len(got) != peExportLimit {
		t.Fatalf("got %d exports, want %d", len(got), peExportLimit)
	}
	if diag != "" {
		t.Errorf("unexpected diagnostics:\n%s", diag)
	}
}

// TestInitDynexportNonPluginTruncates pins the legacy warn-and-truncate
// behavior for build modes whose export lists are bounded upstream (hosts
// bound theirs by rank in peSnapshotHostExports): everything past
// peExportLimit is dropped with a warning naming the counts. Only plugins
// hard-error (trendvidia/go-archive#39).
func TestInitDynexportNonPluginTruncates(t *testing.T) {
	ctxt := newExportCandidates(t, "exe", peExportLimit+2)
	resetExportState(t)
	// cmd/go passes -pluginexename for every Windows link with -o, not
	// just plugin ones, and links to objdir/a.out.exe; the warning has to
	// name the image the user asked for, like the plugin error does.
	*flagPluginExeName = "host.exe"
	var got []loader.Sym
	diag := captureStderr(t, func() {
		initdynexport(ctxt)
		got = dexport
	})
	if len(got) != peExportLimit {
		t.Fatalf("got %d exports after truncation, want %d", len(got), peExportLimit)
	}
	wantWarn := fmt.Sprintf("%d were dropped and cannot be resolved by GetProcAddress", 2)
	if !strings.Contains(diag, wantWarn) {
		t.Errorf("warning %q missing from output:\n%s", wantWarn, diag)
	}
	if !strings.Contains(diag, "host.exe exports") {
		t.Errorf("warning does not name the requested output (-pluginexename=host.exe):\n%s", diag)
	}
	if strings.Contains(diag, "test.dll") {
		t.Errorf("warning names the linker's temp output instead of the requested one:\n%s", diag)
	}
}

// TestPEOutputDisplayName pins both branches of the name diagnostics use:
// the -pluginexename cmd/go passes when it has one, the output-file base
// otherwise. Every over-limit message goes through this, and the
// distinction only shows up when the two differ — which is the normal
// case, since cmd/go links to objdir/a.out.exe and copies afterwards.
func TestPEOutputDisplayName(t *testing.T) {
	resetExportState(t) // saves and restores both flags
	*flagOutfile = filepath.Join("objdir", "a.out.dll")

	if got, want := peOutputDisplayName(), "a.out.dll"; got != want {
		t.Errorf("with no -pluginexename: got %q, want %q", got, want)
	}
	*flagPluginExeName = "big.dll"
	if got, want := peOutputDisplayName(), "big.dll"; got != want {
		t.Errorf("with -pluginexename=big.dll: got %q, want %q", got, want)
	}
}

// TestPEPluginOverLimitLink is the end-to-end pin for trendvidia/go-archive#39's
// chosen policy: a -buildmode=plugin windows link whose reachable
// CgoExportDynamic count exceeds peExportLimit fails at link time with an
// error naming the limit, and leaves no DLL behind. It cross-builds for
// windows/amd64 with internal linking, which needs no C toolchain, so it
// runs on any host.
func TestPEPluginOverLimitLink(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("generates and links a >65500-export plugin fixture; slow")
	}

	goTool := testenv.GoToolPath(t)
	tmpdir := t.TempDir()
	writeFile := func(rel, src string) {
		t.Helper()
		path := filepath.Join(tmpdir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goBuild := func(args ...string) ([]byte, error) {
		t.Helper()
		cmd := testenv.Command(t, goTool, append([]string{"build"}, args...)...)
		cmd.Dir = tmpdir
		// Pin the target: -buildmode=plugin is only supported on
		// windows/{amd64,arm64} (internal/platform), so inheriting the
		// host GOARCH would make this fail for an unrelated reason on
		// any other builder. CGO_ENABLED=0 keeps the "no C toolchain"
		// property real rather than incidental.
		cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
		return cmd.CombinedOutput()
	}

	writeFile("go.mod", "module p39pin\n\ngo 1.26\n")
	writeFile("host/main.go", `package main

import _ "plugin"

func main() {}
`)
	// A plugin over the limit: the filler count alone exceeds
	// peExportLimit, so the total does too regardless of how many
	// wrapper symbols the base image contributes.
	filler := peExportLimit + 50
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := 0; i < filler; i++ {
		fmt.Fprintf(&b, "func F%d() int { return %d }\n", i, i)
	}
	// The package-level Keep root references every filler function, so
	// deadcode keeps them reachable and the internal-linking export walk
	// marks them all.
	b.WriteString("var Keep = []any{F0")
	for i := 1; i < filler; i++ {
		fmt.Fprintf(&b, ",F%d", i)
	}
	b.WriteString("}\n")
	writeFile("big/big.go", b.String())
	writeFile("plugmain/main.go", `package main

import "p39pin/big"

func Use() int { return len(big.Keep) }

func main() {}
`)
	// Control: a small plugin against the same host still links.
	writeFile("small/main.go", `package main

import "p39pin/big"

func Use() int { return big.F0() }

func main() {}
`)

	if out, err := goBuild("-o", filepath.Join("host", "host.exe"), "./host"); err != nil {
		t.Fatalf("building host: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmpdir, "host", "host.exe.a")); err != nil {
		t.Fatalf("host import library missing after host build: %v", err)
	}

	pluginArgs := func(exe string) []string {
		return []string{"-buildmode=plugin",
			"-ldflags=-pluginimplib=" + filepath.Join("host", "host.exe.a"),
			"-o", exe}
	}

	if out, err := goBuild(append(pluginArgs(filepath.Join("small", "small.dll")), "./small")...); err != nil {
		t.Fatalf("control plugin link failed: %v\n%s", err, out)
	}

	// Oversized: fails at link time naming the limit...
	out, err := goBuild(append(pluginArgs("over.dll"), "./plugmain")...)
	if err == nil {
		t.Fatalf("oversized plugin link unexpectedly succeeded:\n%s", out)
	}
	// The two abort sites word this differently on purpose (they bound
	// different sets -- see TestPEExportLimitAborts), so match only the
	// tail they share. This fixture takes the internal path today; if it
	// ever reaches pePluginExportFile, the "pins" tolerance below must
	// not be the only thing that moves.
	wantErr := "more than the " + strconv.Itoa(peExportLimit) +
		" a PE image can address; reduce the exported surface"
	if !strings.Contains(string(out), wantErr) {
		t.Errorf("link failure does not name the limit (%q). Output:\n%s", wantErr, out)
	}
	// It has to name the DLL the user asked for. cmd/go links plugins to
	// objdir/a.out.dll and copies afterwards, so the linker's own output
	// path is a temp file that is gone by the time anyone reads the
	// error; -pluginexename carries the real name.
	if !strings.Contains(string(out), "over.dll exports") && !strings.Contains(string(out), "over.dll pins") {
		t.Errorf("link failure does not name over.dll. Output:\n%s", out)
	}
	if strings.Contains(string(out), "a.out.dll exports") || strings.Contains(string(out), "a.out.dll pins") {
		t.Errorf("link failure names the linker's temp output instead of over.dll. Output:\n%s", out)
	}
	// ...and leaves no artifact behind.
	if _, serr := os.Stat(filepath.Join(tmpdir, "over.dll")); serr == nil {
		t.Error("oversized link left a DLL behind; want deterministic failure with no output")
	}
}

// peExportLimitChildEnv selects which over-limit abort site the child
// process below exercises.
const peExportLimitChildEnv = "GO_TEST_PE_EXPORT_LIMIT_ABORT"

// TestPEExportLimitAborts pins the policy trendvidia/go-archive#39 chose at both
// sites that implement it: over the cap, a plugin link reports the limit
// and ends the process with a failing status.
//
// It re-runs this test binary because the sites call errorexit(), which
// no in-process test survives. That also makes it the only pin on the
// external-linking site (pePluginExportFile is otherwise reached only
// through hostlink, i.e. a mingw/lld link on Windows), and the only pin
// on the internal-linking abort that runs under -short, which is the mode
// all.bash uses.
func TestPEExportLimitAborts(t *testing.T) {
	testenv.MustHaveExec(t)
	for _, mode := range []string{"internal", "external"} {
		t.Run(mode, func(t *testing.T) {
			cmd := testenv.Command(t, os.Args[0], "-test.run=^TestPEExportLimitAbortChild$")
			cmd.Env = append(cmd.Environ(), peExportLimitChildEnv+"="+mode)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("child exited 0; want the over-limit link aborted:\n%s", out)
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Errorf("child failed with %v, want exit status 2:\n%s", err, out)
			}
			// Both messages name the requested DLL and the limit, but
			// they deliberately say different things, because the two
			// sites bound different sets (trendvidia/go-archive#59).
			//
			// initdynexport counts the export table it is about to
			// emit, so "exports N symbols" is literally true there.
			// pePluginExportFile counts only the names it pins in the
			// .def; mingw's auto-export publishes more, so claiming the
			// image "exports" that number is the false confidence trendvidia/go-archive#59
			// was filed about. The cap that binds the external image is
			// peVerifyImageExportCount, after hostlink.
			//
			// Keep the wording distinct. Making these identical again
			// would restore a message that misdescribes what was
			// measured.
			want := fmt.Sprintf("big.dll exports %d symbols, more than the %d a PE image can address; reduce the exported surface",
				peExportLimit+1, peExportLimit)
			if mode == "external" {
				want = fmt.Sprintf("big.dll pins %d symbols in its export .def, more than the %d a PE image can address; reduce the exported surface",
					peExportLimit+1, peExportLimit)
			}
			if !strings.Contains(string(out), want) {
				t.Errorf("child output does not contain %q:\n%s", want, out)
			}
		})
	}
}

// TestPEExportLimitAbortChild is the child half of TestPEExportLimitAborts.
// It is inert unless peExportLimitChildEnv selects a mode.
func TestPEExportLimitAbortChild(t *testing.T) {
	mode := os.Getenv(peExportLimitChildEnv)
	if mode == "" {
		t.Skip("child half of TestPEExportLimitAborts")
	}
	ctxt := newExportCandidates(t, "plugin", peExportLimit+1)
	resetExportState(t)
	*flagPluginExeName = "big.dll"
	switch mode {
	case "internal":
		initdynexport(ctxt)
	case "external":
		oldTmpdir := *flagTmpdir
		*flagTmpdir = t.TempDir()
		t.Cleanup(func() { *flagTmpdir = oldTmpdir })
		pePluginExportFile(ctxt)
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	// Reaching here means the link would have gone on to produce a DLL
	// whose exports the loader cannot address.
	t.Fatalf("%s over-limit link returned instead of aborting", mode)
}

// newExportABIPair returns a windows/amd64 plugin context whose loader
// holds one function present under BOTH ABI versions -- an ABI0 text
// symbol and an ABIInternal text symbol sharing a name -- plus three
// single-ABI functions alongside it: one plain, one whose extname
// differs from its symbol name, and one whose name peSkipSymName drops.
//
// The pair is the shape mangleABIName exists for: it renames the ABI0
// entry to "X.abi0" and leaves the ABIInternal one as "X", so the two
// are distinguishable in an export table. A walk keyed on the raw name
// sees "X" twice instead.
func newExportABIPair(t *testing.T) *Link {
	t.Helper()
	origGOARCH := buildcfg.GOARCH
	buildcfg.GOARCH = sys.ArchAMD64.Name
	// mangleABIName reads the package-level experiment set, not the
	// context's arch, so a host whose GOARCH has no regabi support (or a
	// GOEXPERIMENT=noregabiwrappers build) would otherwise see an
	// unmangled export set and the pin would test nothing. A windows/amd64
	// cross-link from such a host does mangle; set the flag the target
	// implies rather than skipping.
	origExp := buildcfg.Experiment
	buildcfg.Experiment.RegabiWrappers = true
	t.Cleanup(func() {
		buildcfg.GOARCH = origGOARCH
		buildcfg.Experiment = origExp
	})
	ctxt := setUpContext(sys.ArchAMD64, false, objabi.Hwindows, "plugin", "external")
	ldr := ctxt.loader
	for _, v := range []int{sym.SymVerABI0, sym.SymVerABIInternal} {
		sb := ldr.CreateSymForUpdate("p.Both", v)
		sb.SetType(sym.STEXT)
		sb.SetReachable(true)
		ldr.SetAttrCgoExportDynamic(sb.Sym(), true)
	}
	for _, name := range []string{"p.Solo", "p.Renamed", "_p.Underscore"} {
		sb := ldr.CreateSymForUpdate(name, sym.SymVerABIInternal)
		sb.SetType(sym.STEXT)
		sb.SetReachable(true)
		ldr.SetAttrCgoExportDynamic(sb.Sym(), true)
	}
	// What a two-argument //go:cgo_export_dynamic Local Remote leaves
	// behind (go.go): extname is the name the directive asked for, while
	// the object this link emits still defines the symbol under SymName.
	ldr.SetSymExtname(ldr.Lookup("p.Renamed", sym.SymVerABIInternal), "p.RemoteName")
	return ctxt
}

// defNames reads an EXPORTS-format .def and returns the listed names.
func defNames(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "EXPORTS" {
		t.Fatalf(".def does not start with EXPORTS:\n%s", b)
	}
	var out []string
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestPluginExportFileABIPairs pins the naming and de-duplication of the
// .def handed to an external linker (trendvidia/go-archive#53).
//
// A function carrying both ABI versions must appear as "X" and "X.abi0",
// each exactly once. Keyed on the raw symbol name it appeared as "X"
// twice and the ABI0 body was unnameable -- and initdynexport's comment
// records that external-link plugins are precisely the case needing the
// ABI0 export by name, since the host's ABIInternal wrapper clobbers the
// caller's stack argument before tail-calling the ABI0 body.
//
// The other two properties pinned here are the ones the ABI fix must not
// disturb: names come from SymName and not SymExtname, because ld has to
// resolve each EXPORTS line against the object this link emitted; and
// peSkipSymName still runs on that name.
func TestPluginExportFileABIPairs(t *testing.T) {
	ctxt := newExportABIPair(t)
	resetExportState(t)
	oldTmpdir := *flagTmpdir
	*flagTmpdir = t.TempDir()
	t.Cleanup(func() { *flagTmpdir = oldTmpdir })

	fname := pePluginExportFile(ctxt)
	if fname == "" {
		t.Fatal("pePluginExportFile returned no file for a non-empty export set")
	}
	got := defNames(t, fname)

	counts := map[string]int{}
	for _, n := range got {
		counts[n]++
	}
	for _, want := range []string{"p.Both", "p.Both.abi0", "p.Solo", "p.Renamed"} {
		switch counts[want] {
		case 1:
		case 0:
			t.Errorf("%q missing from .def; got %v", want, got)
		default:
			t.Errorf("%q listed %d times, want once; got %v", want, counts[want], got)
		}
	}
	// The extname is what the directive wants the world to call it, but
	// no object in this link defines that name; ld would fail the link
	// with "cannot export p.RemoteName: symbol not found".
	if counts["p.RemoteName"] != 0 {
		t.Errorf(".def names the extname p.RemoteName, which no object defines; got %v", got)
	}
	// peSkipSymName drops leading-underscore names on this path as it
	// does on the other two walks.
	if counts["_p.Underscore"] != 0 {
		t.Errorf(".def lists %q, which peSkipSymName rejects; got %v", "_p.Underscore", got)
	}
	for n, c := range counts {
		if c > 1 {
			t.Errorf("duplicate .def entry %q (x%d)", n, c)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf(".def entries not sorted: %v", got)
	}
}

// TestPluginExportFileEmpty pins the other end of the range: with nothing
// to export, pePluginExportFile writes no file and returns "", which is
// what peExportArgs tests before adding the .def to the link line. A
// stray empty EXPORTS file would make mingw ld export nothing at all.
func TestPluginExportFileEmpty(t *testing.T) {
	ctxt := newExportCandidates(t, "plugin", 0)
	resetExportState(t)
	tmpdir := t.TempDir()
	oldTmpdir := *flagTmpdir
	*flagTmpdir = tmpdir
	t.Cleanup(func() { *flagTmpdir = oldTmpdir })

	if fname := pePluginExportFile(ctxt); fname != "" {
		t.Errorf("pePluginExportFile = %q for an empty export set, want \"\"", fname)
	}
	if _, err := os.Stat(filepath.Join(tmpdir, "plugin_exports.def")); err == nil {
		t.Error("pePluginExportFile wrote a .def for an empty export set")
	}
}
