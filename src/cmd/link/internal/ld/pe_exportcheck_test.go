// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cmd/internal/objabi"
	"cmd/internal/sys"
	"internal/buildcfg"
	"internal/testenv"
)

// peExportingMain is a host that references plugin.Open, which is what
// makes the linker publish the cross-module export table.
const peExportingMain = `package main

import (
	"fmt"
	"plugin"
)

func main() { fmt.Println(plugin.Open) }
`

// peNonExportingMain references nothing that makes the image exportable,
// so it links with no export directory at all -- the state every fixture
// built from peExportingMain can never reach.
const peNonExportingMain = `package main

func main() { println(1) }
`

// buildWindowsPE cross-builds a windows/amd64 image from src and returns
// its path. CGO_ENABLED=0 keeps it on the internal linker, so this runs
// on any host; the import library the linker emits sits alongside it at
// the returned path + ".a".
func buildWindowsPE(t *testing.T, src string) string {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0666); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module pecount\n\ngo 1.26\n")
	write("main.go", src)
	exe := filepath.Join(dir, "host.exe")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe, ".")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

// TestPEImageExportOverLimit pins that the export cap is measured against
// the linked image (trendvidia/go#59).
//
// The check this replaces counted the names written into the plugin's
// .def, which on the external-link path is not the export table: mingw's
// auto-export publishes more, so a .def naming 6 symbols produced an image
// publishing 2195. A cap testing the smaller number can only ever pass,
// which reads as "verified under the limit" when nothing was verified.
//
// The limit is a parameter precisely so this can be demonstrated. Driven
// only at the real 65500 the test could never distinguish "counts the
// image" from "counts something smaller that happens to fit".
func TestPEImageExportOverLimit(t *testing.T) {
	t.Parallel()
	exe := buildWindowsPE(t, peExportingMain)

	n, over, err := peImageExportOverLimit(exe, 1<<30)
	if err != nil {
		t.Fatalf("peImageExportOverLimit: %v", err)
	}
	if n == 0 {
		t.Fatal("image publishes no exports; the fixture cannot demonstrate anything")
	}
	if over {
		t.Errorf("image with %d exports reported over a limit of 1<<30", n)
	}

	// A limit below the real count must fire, and the count reported must
	// be the image's — not the size of any list the linker built.
	if _, over, err := peImageExportOverLimit(exe, n-1); err != nil {
		t.Fatal(err)
	} else if !over {
		t.Errorf("image with %d exports not reported over a limit of %d", n, n-1)
	}

	// Exactly at the limit fits: the format bounds the count, so the
	// boundary is > and not >=.
	if _, over, err := peImageExportOverLimit(exe, n); err != nil {
		t.Fatal(err)
	} else if over {
		t.Errorf("image with %d exports reported over a limit of %d", n, n)
	}
}

// TestPEImageExportNamesRejectsNonPE pins that an unreadable or non-PE
// output is reported rather than silently counted as zero exports — a
// zero would sail under any limit and look like a pass.
func TestPEImageExportNamesRejectsNonPE(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	junk := filepath.Join(dir, "not-a-pe.dll")
	if err := os.WriteFile(junk, []byte("this is not a PE image"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := peImageExportNames(junk); err == nil {
		t.Error("peImageExportNames accepted a non-PE file")
	}
	if _, err := peImageExportNames(filepath.Join(dir, "absent.dll")); err == nil {
		t.Error("peImageExportNames accepted a missing file")
	}
}

// peSetExportFunctionCount copies the image at path and rewrites its
// export directory's NumberOfFunctions field, returning the copy.
//
// That field is the only way to build the state no Go-linked fixture can
// reach: the internal linker names every export, so NumberOfNames and
// NumberOfFunctions are always equal in anything this package can build.
// A NONAME or ordinal-only export -- which mingw's .def parser and
// lld-link both accept -- grows the address table without growing the
// name table.
func peSetExportFunctionCount(t *testing.T, path string, n uint32) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		t.Fatalf("%s: not a PE32+ image", path)
	}
	rva := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_EXPORT].VirtualAddress
	var off uint32
	for _, sect := range f.Sections {
		if sect.VirtualAddress <= rva && rva < sect.VirtualAddress+sect.VirtualSize {
			off = sect.Offset + (rva - sect.VirtualAddress)
			break
		}
	}
	if off == 0 {
		t.Fatalf("%s: export directory RVA %#x is in no section", path, rva)
	}
	// NumberOfFunctions sits at offset 20 of IMAGE_EXPORT_DIRECTORY.
	binary.LittleEndian.PutUint32(data[off+20:], n)
	out := filepath.Join(t.TempDir(), "patched.exe")
	if err := os.WriteFile(out, data, 0666); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPEImageExportOverLimitCountsOrdinalSpace pins that the cap is
// measured against the export address table rather than the name table.
//
// peExportLimit exists because "PE export ordinals are uint16"
// (initdynexport), so the set it bounds is the ordinal space, and every
// address-table entry burns an ordinal whether a name points at it or
// not. Counting names undercounts an image with NONAME exports -- the
// same shape of mistake as #59's .def count, one field over.
func TestPEImageExportOverLimitCountsOrdinalSpace(t *testing.T) {
	t.Parallel()
	exe := buildWindowsPE(t, peExportingMain)

	named, _, err := peImageExportOverLimit(exe, 1<<30)
	if err != nil {
		t.Fatalf("peImageExportOverLimit: %v", err)
	}
	if named == 0 {
		t.Fatal("image publishes no exports; the fixture cannot demonstrate anything")
	}

	// Seven ordinals with no names behind them.
	patched := peSetExportFunctionCount(t, exe, uint32(named+7))
	n, over, err := peImageExportOverLimit(patched, 1<<30)
	if err != nil {
		t.Fatalf("peImageExportOverLimit on patched image: %v", err)
	}
	if n != named+7 {
		t.Errorf("counted %d exports, want %d: the cap is reading the name table, not the ordinal space", n, named+7)
	}
	if over {
		t.Errorf("image with %d exports reported over a limit of 1<<30", n)
	}
	// The unnamed ordinals must be what pushes it over.
	if _, over, err := peImageExportOverLimit(patched, named); err != nil {
		t.Fatal(err)
	} else if !over {
		t.Errorf("image addressing %d ordinals passed a limit of %d", named+7, named)
	}
}

// peVerifyContext returns a windows/amd64 external-link plugin context
// and isolates the linker globals peVerifyImageExportCount reads and
// writes: *flagOutfile, -pluginexename and nerrors.
func peVerifyContext(t *testing.T) *Link {
	t.Helper()
	// linknew requires buildcfg.GOARCH to match the target arch; adopt
	// data_test.go's save-and-set so the fixture stays windows/amd64 on
	// any host.
	origGOARCH := buildcfg.GOARCH
	buildcfg.GOARCH = sys.ArchAMD64.Name
	t.Cleanup(func() { buildcfg.GOARCH = origGOARCH })
	resetExportState(t)
	origErrors := nerrors
	t.Cleanup(func() { nerrors = origErrors })
	nerrors = 0
	return setUpContext(sys.ArchAMD64, false, objabi.Hwindows, "plugin", "external")
}

// TestPEVerifyImageExportCountUnreadableImage pins that a measurement
// failure warns and leaves the link alone.
//
// The function's own comment says a cap check must not break a link that
// otherwise succeeded, and Errorf cannot honour that: util.go states that
// "logging an error means that on exit cmd/link will delete any output
// file and return a non-zero error code", and Main's closing errorexit()
// does exactly that. So any image this walk cannot parse -- an export
// directory in no section, name strings outside the section holding it --
// would turn a good mingw or lld-link plugin into a failed build with no
// artifact.
func TestPEVerifyImageExportCountUnreadableImage(t *testing.T) {
	ctxt := peVerifyContext(t)
	junk := filepath.Join(t.TempDir(), "junk.dll")
	if err := os.WriteFile(junk, []byte("this is not a PE image"), 0666); err != nil {
		t.Fatal(err)
	}
	*flagOutfile = junk

	out := captureStderr(t, func() { peVerifyImageExportCount(ctxt) })
	if nerrors != 0 {
		t.Errorf("nerrors = %d after an unparseable image; want 0, since an error deletes the output and exits 2", nerrors)
	}
	if !strings.Contains(out, "cannot verify PE export count") {
		t.Errorf("no diagnostic for an unmeasurable image; a silent pass reads as a verified one. Output:\n%s", out)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Errorf("output image gone after a failed measurement: %v", err)
	}
}

// TestPEVerifyImageExportCountAfterError pins that the check stays quiet
// once something else has failed. hostlink returns without running the
// external linker when nerrors > 0 (lib.go), so *flagOutfile names a file
// that was never written, and measuring it would report "cannot verify"
// against a failure that happened somewhere else.
func TestPEVerifyImageExportCountAfterError(t *testing.T) {
	ctxt := peVerifyContext(t)
	nerrors = 1
	*flagOutfile = filepath.Join(t.TempDir(), "never-written.dll")

	out := captureStderr(t, func() { peVerifyImageExportCount(ctxt) })
	if out != "" {
		t.Errorf("export check reported on a link that had already failed:\n%s", out)
	}
	if nerrors != 1 {
		t.Errorf("nerrors = %d, want 1: the export check added an error of its own", nerrors)
	}
}

// TestPEVerifyImageExportCountEmptyExports pins that an image with no
// export directory is reported rather than counted as a pass. Zero sails
// under any limit, and a plugin that publishes nothing is one the host
// cannot bind a single symbol out of -- logging "publishes 0 of 65500" is
// the same false confidence #59 was filed about.
func TestPEVerifyImageExportCountEmptyExports(t *testing.T) {
	ctxt := peVerifyContext(t)
	exe := buildWindowsPE(t, peNonExportingMain)
	if names, err := peImageExportNames(exe); err != nil {
		t.Fatalf("peImageExportNames: %v", err)
	} else if len(names) != 0 {
		t.Skipf("fixture publishes %d exports; it cannot exercise the empty case", len(names))
	}
	*flagOutfile = exe

	out := captureStderr(t, func() { peVerifyImageExportCount(ctxt) })
	if !strings.Contains(out, "publishes no exports") {
		t.Errorf("an image with an empty export directory verified silently. Output:\n%s", out)
	}
	if nerrors != 0 {
		t.Errorf("nerrors = %d; the empty case warns, it does not fail the link", nerrors)
	}
}
