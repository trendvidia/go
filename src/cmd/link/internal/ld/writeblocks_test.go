// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cmd/internal/objabi"
	"cmd/internal/sys"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"internal/buildcfg"
	"internal/testenv"
)

// writeBlocksChildEnv arms the child halves of the abort tests below.
const writeBlocksChildEnv = "GO_TEST_WRITEBLOCKS_UNSORTED_ABORT"

// runAbortChild re-runs this test binary in the named child test — the
// diagnostics under test call errorexit(), which no in-process test
// survives — and returns its combined output, having checked that it died
// the way a real link dies rather than by panicking.
func runAbortChild(t *testing.T, child string) string {
	t.Helper()
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, os.Args[0], "-test.run=^"+child+"$")
	cmd.Env = append(cmd.Environ(), writeBlocksChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child %s exited 0; want the unsorted input aborted:\n%s", child, out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Errorf("child %s failed with %v, want exit status 2:\n%s", child, err, out)
	}
	// Both hazards only ever manifested as a runtime panic (which also
	// exits 2); the pin is that each is a named diagnostic instead.
	if strings.Contains(string(out), "runtime error") {
		t.Errorf("child %s hit a runtime panic; want a linker diagnostic:\n%s", child, out)
	}
	return string(out)
}

// setUpWindowsAMD64Context builds a windows/amd64 link context on any
// host. setUpContext insists buildcfg.GOARCH match the arch it is given,
// so swap it for the duration of the test.
func setUpWindowsAMD64Context(t *testing.T) *Link {
	t.Helper()
	origGOARCH := buildcfg.GOARCH
	buildcfg.GOARCH = sys.ArchAMD64.Name
	t.Cleanup(func() { buildcfg.GOARCH = origGOARCH })
	return setUpContext(sys.ArchAMD64, false, objabi.Hwindows, "exe", "internal")
}

// newAbortContext builds the minimal windows/amd64 link context the child
// tests drive writeBlocks/writeBlock with.
func newAbortContext(t *testing.T) *Link {
	t.Helper()
	ctxt := setUpWindowsAMD64Context(t)
	// setUpContext's loader has no AfterErrorAction, so Errorf would not
	// feed nerrors and errorexit would exit 0; wire the production
	// reporter so the child's exit status matches a real link's.
	ctxt.loader = loader.NewLoader(0, &loader.ErrorReporter{AfterErrorAction: afterErrorAction})
	return ctxt
}

// TestWriteBlocksUnsortedAborts pins the boundary where trendvidia/go-archive#49's
// layout accounting first broke: writeBlocks trusts its symbol list to be
// sorted by address, and a symbol whose value lies below the write cursor
// used to drive the block length negative, wrap the output offset by the
// image base, and panic in OutBuf.WriteSym. The invariant now fails the
// link with a named phase error instead. The unsorted input this pins is
// exactly what dodata used to produce on Windows: a section-less STLSBSS
// symbol (runtime.tlsg, value ~0) appended after the allocated data
// symbols.
func TestWriteBlocksUnsortedAborts(t *testing.T) {
	out := runAbortChild(t, "TestWriteBlocksUnsortedAbortChild")
	want := "phase error: writeBlocks: next symbol has val=0x0 before current addr="
	if !strings.Contains(out, want) {
		t.Errorf("child output does not contain %q:\n%s", want, out)
	}
}

// TestWriteBlocksUnsortedAbortChild is the child half of
// TestWriteBlocksUnsortedAborts. It is inert unless writeBlocksChildEnv
// arms it.
func TestWriteBlocksUnsortedAbortChild(t *testing.T) {
	if os.Getenv(writeBlocksChildEnv) == "" {
		t.Skip("child half of TestWriteBlocksUnsortedAborts")
	}
	ctxt := newAbortContext(t)
	ldr := ctxt.loader

	// One data symbol whose end crosses the blockSize boundary, so the
	// block scan breaks with the next symbol still pending, followed by
	// a section-less near-zero-value symbol standing in for the
	// runtime.tlsg that dodata used to leave at datap's tail.
	const addr = 0x1000
	big := ldr.CreateSymForUpdate("p.big", 0)
	big.SetType(sym.SDATA)
	big.SetSize(blockSize + 0x100)
	ldr.SetSymValue(big.Sym(), addr)
	low := ldr.CreateSymForUpdate("p.low", 0)
	low.SetType(sym.STLSBSS)
	low.SetSize(8)
	ldr.SetSymValue(low.Sym(), 0)

	syms := []loader.Sym{big.Sym(), low.Sym()}
	writeBlocks(ctxt, ctxt.Out, ctxt.outSem, ldr, syms, addr, blockSize+0x200, zeros[:])

	// Reaching here means the negative block length went undiagnosed —
	// the state that used to wrap the write offset and panic.
	t.Fatal("writeBlocks returned on unsorted input instead of aborting")
}

// TestWriteBlockSectionlessSymAborts covers the other arm of the same
// hazard. writeBlocks' guard only sees a below-cursor symbol when it is the
// one that *starts* the next block; when it is instead swept into a block
// (any geometry where the last real symbol ends before eaddr, e.g. the
// alignment gap between .data and .bss), the symbol reaches writeBlock's
// own "addr=... but val=..." check. That check used to print
// ldr.SymSect(s).Name, and SymSect is nil for exactly the symbols that get
// here: nil is a symbol's declared initial state, and address() never
// assigns a section to TLS BSS on a non-ELF target. So the linker died with
// a nil-pointer panic inside its own diagnostic. It must report instead.
func TestWriteBlockSectionlessSymAborts(t *testing.T) {
	out := runAbortChild(t, "TestWriteBlockSectionlessSymAbortChild")
	want := "phase error: addr=0x1000 but val=0x0 sym=p.low"
	if !strings.Contains(out, want) {
		t.Errorf("child output does not contain %q:\n%s", want, out)
	}
	if !strings.Contains(out, "sect=<nil>") {
		t.Errorf("child output does not report the missing section as sect=<nil>:\n%s", out)
	}
}

// TestWriteBlockSectionlessSymAbortChild is the child half of
// TestWriteBlockSectionlessSymAborts. It is inert unless
// writeBlocksChildEnv arms it.
func TestWriteBlockSectionlessSymAbortChild(t *testing.T) {
	if os.Getenv(writeBlocksChildEnv) == "" {
		t.Skip("child half of TestWriteBlockSectionlessSymAborts")
	}
	ctxt := newAbortContext(t)
	ldr := ctxt.loader

	// Deliberately no SetSymSect: an unset section is where every symbol
	// starts, and where TLS BSS stays on every non-ELF target.
	low := ldr.CreateSymForUpdate("p.low", 0)
	low.SetType(sym.STLSBSS)
	low.SetSize(8)
	ldr.SetSymValue(low.Sym(), 0)
	if ldr.SymSect(low.Sym()) != nil {
		t.Fatal("p.low unexpectedly has a section; the nil case is what this pins")
	}

	writeBlock(ctxt, ctxt.Out, ldr, []loader.Sym{low.Sym()}, 0x1000, 0x1000, zeros[:])

	t.Fatal("writeBlock returned on a below-cursor symbol instead of aborting")
}

// TestDatapExcludesTLSBSS pins the fix itself in the mode all.bash runs.
// TestPEPluginLargeDataLinks covers the same exclusion end to end, but it
// skips under -short and cmd/dist runs this package short, so without an
// in-process pin nothing in the default build fails the day the exclusion
// is dropped (an upstream merge restoring the original loop, say) and #49
// returns.
func TestDatapExcludesTLSBSS(t *testing.T) {
	ctxt := setUpWindowsAMD64Context(t)
	ldr := ctxt.loader
	mk := func(name string, kind sym.SymKind, val int64) loader.Sym {
		s := ldr.CreateSymForUpdate(name, 0)
		s.SetType(kind)
		s.SetSize(8)
		ldr.SetSymValue(s.Sym(), val)
		return s.Sym()
	}
	state := &dodataState{ctxt: ctxt}
	state.data[sym.SDATA] = []loader.Sym{mk("p.data", sym.SDATA, 0x2000)}
	state.data[sym.SBSS] = []loader.Sym{mk("p.bss", sym.SBSS, 0x3000)}
	state.data[sym.STLSBSS] = []loader.Sym{mk("runtime.tlsg", sym.STLSBSS, 0)}

	// datap must hold the allocated data in address order and nothing
	// else: runtime.tlsg's value is a TLS-block offset, not an image
	// address, so including it makes datap unsorted at its tail.
	check := func(what string, want []string) {
		t.Helper()
		state.buildDatap(ctxt)
		var got []string
		for _, s := range ctxt.datap {
			got = append(got, ldr.SymName(s))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: datap = %v, want %v", what, got, want)
		}
		// The sizing loop and the append loop must skip the same kinds,
		// or datap reallocates (harmless) or is over-reserved (a silent
		// sign the two loops have drifted apart).
		if cap(ctxt.datap) != len(want) {
			t.Errorf("%s: datap cap = %d, want %d: the sizing and append loops must skip the same kinds", what, cap(ctxt.datap), len(want))
		}
	}
	check("with a TLS BSS symbol", []string{"p.data", "p.bss"})

	// The ordinary case: a target with no TLS BSS symbol at all, which is
	// the initial state of every state.data bucket. The skip must leave
	// it alone.
	state.data[sym.STLSBSS] = nil
	check("with no TLS BSS symbol", []string{"p.data", "p.bss"})
}

// TestPEPluginLargeDataLinks pins trendvidia/go-archive#49's real-world geometry:
// a windows plugin whose .bss and .noptrbss are empty (its runtime data
// lives in the host) and whose largest .data symbol spans a blockSize
// boundary. Before excluding STLSBSS from datap, this link panicked in
// writeBlocks ("slice bounds out of range [-53687...]"), regardless of the
// export count. Like TestPEPluginOverLimitLink it cross-builds for
// windows/amd64 with internal linking, so it runs on any host.
func TestPEPluginLargeDataLinks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("builds a windows host and a plugin with >1MB of data; slow")
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
		cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
		return cmd.CombinedOutput()
	}

	writeFile("go.mod", "module p49pin\n\ngo 1.26\n")
	writeFile("host/main.go", `package main

import _ "plugin"

func main() {}
`)
	// Keep is a >1MB funcval array: its pointer-carrying backing store is
	// the plugin's largest .data symbol, so dodata sorts it last among
	// the allocated data and a 1MB writeBlocks boundary lands inside it.
	// 100000 entries * 16 bytes = 1.6MB leaves margin on both sides of
	// the boundary. The export count stays four orders of magnitude
	// under peExportLimit: the panic geometry never needed the cap.
	var b strings.Builder
	b.WriteString("package big\n\nfunc F0() int { return 0 }\n\nvar Keep = []any{\n")
	for i := 0; i < 100000; i++ {
		b.WriteString("F0,\n")
	}
	b.WriteString("}\n")
	writeFile("big/big.go", b.String())
	writeFile("plugmain/main.go", `package main

import "p49pin/big"

func Use() int { return len(big.Keep) }

func main() {}
`)

	if out, err := goBuild("-o", filepath.Join("host", "host.exe"), "./host"); err != nil {
		t.Fatalf("building host: %v\n%s", err, out)
	}
	dll := filepath.Join(tmpdir, "large.dll")
	out, err := goBuild("-buildmode=plugin",
		"-ldflags=-pluginimplib="+filepath.Join("host", "host.exe.a"),
		"-o", dll, "./plugmain")
	if err != nil {
		t.Fatalf("large-data plugin link failed: %v\n%s", err, out)
	}
	fi, err := os.Stat(dll)
	if err != nil {
		t.Fatalf("large-data plugin left no DLL: %v", err)
	}
	// The DLL must actually carry the array (the geometry under test),
	// not have dead-coded it away.
	if min := int64(1 << 20); fi.Size() < min {
		t.Errorf("large.dll is %d bytes, want at least %d (did the >1MB data symbol survive?)", fi.Size(), min)
	}
}
