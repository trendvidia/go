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

	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0666); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module pehost\n\ngo 1.26\n")
	write("main.go", `package main

import (
	"fmt"
	"plugin"
)

func main() { fmt.Println(plugin.Open) }
`)

	exe := filepath.Join(dir, "host.exe")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe, ".")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

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

// peExportedNames returns the names in a PE image's export directory.
func peExportedNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var dd []pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		dd = oh.DataDirectory[:]
	case *pe.OptionalHeader32:
		dd = oh.DataDirectory[:]
	default:
		t.Fatalf("%s: no optional header", path)
	}
	ed := dd[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
	if ed.Size == 0 {
		return nil
	}

	var sect *pe.Section
	for _, s := range f.Sections {
		if s.VirtualAddress <= ed.VirtualAddress && ed.VirtualAddress < s.VirtualAddress+s.VirtualSize {
			sect = s
			break
		}
	}
	if sect == nil {
		t.Fatalf("%s: export directory RVA %#x is in no section", path, ed.VirtualAddress)
	}
	data, err := sect.Data()
	if err != nil {
		t.Fatal(err)
	}
	at := func(rva uint32) []byte {
		off := rva - sect.VirtualAddress
		if off >= uint32(len(data)) {
			t.Fatalf("%s: RVA %#x outside %s", path, rva, sect.Name)
		}
		return data[off:]
	}

	hdr := at(ed.VirtualAddress)
	nNames := binary.LittleEndian.Uint32(hdr[24:])
	namePtrs := at(binary.LittleEndian.Uint32(hdr[32:]))
	names := make([]string, 0, nNames)
	for i := uint32(0); i < nNames; i++ {
		s := at(binary.LittleEndian.Uint32(namePtrs[i*4:]))
		names = append(names, string(s[:bytes.IndexByte(s, 0)]))
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
