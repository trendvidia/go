// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"internal/pluginabi"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The testplugin suite exercises the ABI fence only through real,
// completely well-formed DLLs produced by this toolchain: every fixture
// there has a valid section table, an export directory of the true
// size, and arrays that all resolve. That leaves every structural
// rejection path in checkABIMarker unexercised — which is where its
// defects have been (a bounds bug rejected every DLL, and a strict
// PointerToRawData check rejected every cgo DLL). These fixtures are
// synthetic PE32+ images built byte by byte so the malformed,
// hostile and unusual-but-legitimate shapes are covered too.

const (
	peSigOff  = 0x40 // e_lfanew
	optSize   = 240  // SizeOfOptionalHeader for PE32+
	rdataPtr  = 0x200
	rdataRVA  = 0x1000
	edataPtr  = 0x400
	edataRVA  = 0x2000
	textPtr   = 0x600 // loadablePE's .text raw bytes (past .edata)
	textRVA   = 0x3000
	bssRVA    = 0x3000
	imageSize = 0x800
)

// peImage is a well-formed, single-export PE32+ image. Each test case
// mutates one documented field of a fresh copy, so every fixture stays
// exactly one step away from a binary the fence must accept.
type peImage struct {
	buf []byte

	coff      int // COFF header offset
	optOff    int // optional header offset
	sectTbl   int // first section header
	markerOff int // file offset of the {Marker, Version} pair
	expDir    int // file offset of the export directory
	funcsOff  int // file offset of AddressOfFunctions[0]
	ordsOff   int // file offset of AddressOfNameOrdinals[0]
	nameOff   int // file offset of the exported name string
}

func (p *peImage) u16(off int, v uint16) {
	p.buf[off] = byte(v)
	p.buf[off+1] = byte(v >> 8)
}

func (p *peImage) u32(off int, v uint32) {
	p.buf[off] = byte(v)
	p.buf[off+1] = byte(v >> 8)
	p.buf[off+2] = byte(v >> 16)
	p.buf[off+3] = byte(v >> 24)
}

// sectionHeader fills section header i. raw/ptr of zero describe an
// unbacked section, the shape .bss legitimately takes.
func (p *peImage) sectionHeader(i int, name string, va, vsize, raw, ptr uint32) {
	h := p.sectTbl + i*40
	copy(p.buf[h:h+8], name)
	p.u32(h+8, vsize)
	p.u32(h+12, va)
	p.u32(h+16, raw)
	p.u32(h+20, ptr)
}

// newPE builds an image exporting go:plugin.abi with this toolchain's
// marker. nsect is the number of section headers to write (2, or 3 to
// add the unbacked .bss).
func newPE(nsect int) *peImage {
	p := &peImage{buf: make([]byte, imageSize)}
	p.coff = peSigOff + 4
	p.optOff = p.coff + 20
	p.sectTbl = p.optOff + optSize
	p.markerOff = rdataPtr
	p.expDir = edataPtr
	p.funcsOff = edataPtr + 40
	p.ordsOff = edataPtr + 48
	p.nameOff = edataPtr + 50

	copy(p.buf, "MZ")
	p.u32(0x3c, peSigOff)
	copy(p.buf[peSigOff:], "PE\x00\x00")
	p.u16(p.coff, 0x8664) // Machine: AMD64
	p.u16(p.coff+2, uint16(nsect))
	p.u16(p.coff+16, optSize)     // SizeOfOptionalHeader
	p.u16(p.optOff, 0x20b)        // IMAGE_NT_OPTIONAL_HDR64_MAGIC
	p.u32(p.optOff+112, edataRVA) // data directory[0]: export table RVA
	p.u32(p.optOff+116, 0x200)    // ... and its size

	p.sectionHeader(0, ".rdata", rdataRVA, 0x200, 0x200, rdataPtr)
	p.sectionHeader(1, ".edata", edataRVA, 0x200, 0x200, edataPtr)
	if nsect > 2 {
		// The unbacked shape this fork's own PE writer emits: a rounded
		// SizeOfRawData with PointerToRawData == 0.
		p.sectionHeader(2, ".bss", bssRVA, 0x1000, 0x1000, 0)
	}

	// The exported value: {Marker, Version}, in .rdata.
	p.u32(p.markerOff, pluginabi.Marker)
	p.u32(p.markerOff+4, pluginabi.Version)

	// Export directory, in .edata.
	p.u32(p.expDir+20, 1)           // NumberOfFunctions
	p.u32(p.expDir+24, 1)           // NumberOfNames
	p.u32(p.expDir+28, edataRVA+40) // AddressOfFunctions
	p.u32(p.expDir+32, edataRVA+44) // AddressOfNames
	p.u32(p.expDir+36, edataRVA+48) // AddressOfNameOrdinals
	p.u32(p.funcsOff, rdataRVA)     // functions[0] -> the marker
	p.u32(edataPtr+44, edataRVA+50) // names[0] -> the name string
	p.u16(p.ordsOff, 0)             // ordinals[0]
	copy(p.buf[p.nameOff:], "go:plugin.abi\x00")
	return p
}

// checkWithin runs the fence and fails loudly if it does not return
// promptly. A regression that drops the bound on NumberOfNames would
// otherwise surface as an unexplained package timeout.
func checkWithin(t *testing.T, path string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- checkABIMarker(path) }()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("checkABIMarker did not return within 60s: an unbounded export-table walk is back")
		return nil
	}
}

func TestCheckABIMarker(t *testing.T) {
	tests := []struct {
		name    string
		build   func() []byte
		wantErr string // "" means the image must be accepted
	}{
		{
			name:  "well formed",
			build: func() []byte { return newPE(2).buf },
		},
		{
			// The regression commit 13317ee199 fixed, pinned: a section
			// the fence never needs to resolve must not condemn the image.
			name:  "unbacked bss section",
			build: func() []byte { return newPE(3).buf },
		},
		{
			name: "version from another generation",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.markerOff+4, pluginabi.Version+1)
				return p.buf
			},
			wantErr: "incompatible toolchain ABI",
		},
		{
			name: "corrupted marker word",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.markerOff, pluginabi.Marker^1)
				return p.buf
			},
			wantErr: "unrecognised go:plugin.abi marker",
		},
		{
			name: "export renamed",
			build: func() []byte {
				p := newPE(2)
				p.buf[p.nameOff] = 'x'
				return p.buf
			},
			wantErr: "missing go:plugin.abi",
		},
		{
			name: "no export directory",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.optOff+112, 0)
				return p.buf
			},
			wantErr: "missing go:plugin.abi",
		},
		{
			name: "export directory outside every section",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.optOff+112, 0x900000)
				return p.buf
			},
			wantErr: "unreadable export directory",
		},
		{
			name: "marker value in an unbacked section",
			build: func() []byte {
				p := newPE(3)
				p.u32(p.funcsOff, bssRVA)
				return p.buf
			},
			wantErr: "go:plugin.abi marker is unreadable",
		},
		{
			name: "ordinal past the address table",
			build: func() []byte {
				p := newPE(2)
				p.u16(p.ordsOff, 5)
				return p.buf
			},
			wantErr: "go:plugin.abi marker is unreadable",
		},
		{
			// The hostile shape: a count no file could back, with no
			// name that could end the walk early. Unbounded, this spins
			// ~4e9 iterations of a linear section scan before deciding
			// anything — the fence hanging Open rather than judging it.
			name: "hostile NumberOfNames",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.expDir+24, 0xffffffff)
				p.buf[p.nameOff] = 'x'
				return p.buf
			},
			wantErr: "malformed export directory",
		},
		{
			name: "hostile NumberOfFunctions",
			build: func() []byte {
				p := newPE(2)
				p.u32(p.expDir+20, 0xffffffff)
				return p.buf
			},
			wantErr: "malformed export directory",
		},
		{
			name: "section table past end of file",
			build: func() []byte {
				p := newPE(2)
				p.u16(p.coff+2, 0x2000)
				return p.buf
			},
			wantErr: "malformed section table",
		},
		{
			name: "not a PE image",
			build: func() []byte {
				p := newPE(2)
				p.buf[0] = 'X'
				return p.buf
			},
			wantErr: "not a PE image",
		},
		{
			name: "PE signature past end of file",
			build: func() []byte {
				p := newPE(2)
				p.u32(0x3c, 0x900000)
				return p.buf
			},
			wantErr: "not a PE image",
		},
		{
			name: "32-bit optional header",
			build: func() []byte {
				p := newPE(2)
				p.u16(p.optOff, 0x10b)
				return p.buf
			},
			wantErr: "not a PE32+ image",
		},
		{
			name:    "truncated file",
			build:   func() []byte { return newPE(2).buf[:0x20] },
			wantErr: "not a PE image",
		},
		{
			name:    "empty file",
			build:   func() []byte { return nil },
			wantErr: "not a PE image",
		},
	}

	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".dll")
			if err := os.WriteFile(path, tt.build(), 0o666); err != nil {
				t.Fatal(err)
			}
			err := checkWithin(t, path)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("checkABIMarker = %v; want acceptance", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("checkABIMarker accepted the image; want %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("checkABIMarker = %v; want %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckABIMarkerUnreadable pins the one case that must NOT be
// fenced: a path the OS cannot open at all falls through so
// LoadLibrary reports its own canonical error, exactly as it did
// before the fence existed.
func TestCheckABIMarkerUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.dll")
	if err := checkABIMarker(path); err != nil {
		t.Fatalf("checkABIMarker(%q) = %v; want nil so LoadLibrary reports the OS error", path, err)
	}
}

// loadablePE adapts newPE's synthetic image for an actual LoadLibrary:
// the byte-level fixtures above are only ever parsed by checkABIMarker,
// but the stage-2 checks judge MAPPED modules, so the image needs the
// fields a real loader validates — alignments, SizeOfImage,
// NumberOfRvaAndSizes, readable sections, an executable section, the
// DLL characteristic — and a machine type matching the running
// process. Returns the loaded handle; the caller must FreeLibrary it.
// A loader refusal comes back as an error rather than a skip so that
// exactly one caller — the probe in TestCheckLoadedABIMarker — decides
// whether the synthetic shape is loadable at all; letting each case
// skip on its own would let a stricter loader empty the accept/reject
// matrix while the suite stayed green. The kernel32 subtest keeps the
// essential missing-marker case universal either way.
func loadablePE(t *testing.T, mutate func(p *peImage)) (syscall.Handle, error) {
	t.Helper()
	p := newPE(3)
	switch runtime.GOARCH {
	case "arm64":
		p.u16(p.coff, 0xaa64) // IMAGE_FILE_MACHINE_ARM64
	case "amd64":
		// 0x8664 is what newPE already writes.
	default:
		t.Skipf("no machine type mapped for %s", runtime.GOARCH)
	}
	// Repurpose newPE's third section header (.bss) as a real, backed,
	// executable .text: loaders are stricter about images with no
	// executable section than the byte-level fence ever needs to be,
	// and every real DLL has one.
	p.sectionHeader(2, ".text", textRVA, 0x200, 0x200, textPtr)
	p.u32(p.sectTbl+2*40+36, 0x60000020) // CODE | MEM_EXECUTE | MEM_READ
	p.u32(p.optOff+4, 0x200)             // SizeOfCode
	p.u32(p.optOff+20, textRVA)          // BaseOfCode
	p.u32(p.optOff+24, 0x10000000)       // ImageBase low dword (high dword stays 0); free base needs no relocations
	p.u32(p.optOff+32, 0x1000)           // SectionAlignment
	p.u32(p.optOff+36, 0x200)            // FileAlignment
	// The version fields below are what a well-formed PE32+ DLL carries,
	// not a cure. Windows 11 arm64 still answers "%1 is not a valid
	// Win32 application" for this image, so the value cases below skip
	// there; measured as refused, individually and cumulatively: OS and
	// subsystem versions 6.0, 6.1 and 10.0, a non-zero linker version,
	// an entry point, SizeOfInitializedData, explicit stack and heap
	// sizes, DllCharacteristics 0x8100 and 0x0160, and a .reloc section
	// holding a terminating empty relocation block. Read
	// trendvidia/go#48 before spending another round on header tweaks.
	p.u16(p.optOff+40, 6)      // MajorOperatingSystemVersion (6.0 = Vista, under any host of this fork)
	p.u16(p.optOff+48, 6)      // MajorSubsystemVersion
	p.u32(p.optOff+56, 0x4000) // SizeOfImage (covers .text ending at RVA 0x3200, multiple of SectionAlignment)
	p.u32(p.optOff+60, 0x200)  // SizeOfHeaders
	p.u16(p.optOff+68, 3)      // Subsystem: IMAGE_SUBSYSTEM_WINDOWS_CUI
	p.u32(p.optOff+108, 16)    // NumberOfRvaAndSizes (the export directory lives in slot 0)
	// Without the DLL bit the loader reads the image as an EXE and
	// rejects it ("not a valid Win32 application") before any of the
	// header fields above are even consulted.
	p.u16(p.coff+18, 0x2022) // Characteristics: EXECUTABLE_IMAGE | LARGE_ADDRESS_AWARE | DLL
	for i := 0; i < 2; i++ { // .rdata/.edata: CNT_INITIALIZED_DATA | MEM_READ
		p.u32(p.sectTbl+i*40+36, 0x40000040)
	}
	if mutate != nil {
		mutate(p)
	}
	path := filepath.Join(t.TempDir(), "loadable.dll")
	if err := os.WriteFile(path, p.buf, 0o666); err != nil {
		t.Fatal(err)
	}
	return syscall.LoadLibrary(path)
}

// TestCheckLoadedABIMarker exercises the stage-2 half of the fence
// against genuinely MAPPED modules — the state no file-byte fixture can
// reach, since making stage 1 pass while stage 2 fails requires the
// file to change between the read and the load. The rejection texts
// pinned here feed open()'s post-load refusal path verbatim.
func TestCheckLoadedABIMarker(t *testing.T) {
	t.Run("mapped module without marker", func(t *testing.T) {
		h, err := syscall.LoadLibrary("kernel32.dll")
		if err != nil {
			t.Skipf("kernel32 unavailable: %v", err)
		}
		defer syscall.FreeLibrary(h)
		err = checkLoadedABIMarker(h)
		if err == nil || !strings.Contains(err.Error(), "exports no go:plugin.abi marker") {
			t.Fatalf("checkLoadedABIMarker(kernel32) = %v; want the missing-marker rejection", err)
		}
	})
	// One probe decides the whole value-level set. Either the running
	// loader accepts the synthetic shape — and then every case below
	// must load, a refusal there being a real failure — or none of them
	// can, and the set skips once with the loader's reason. Per-case
	// skipping would let a stricter loader leave checkLoadedABIMarker's
	// accept/reject matrix entirely unexercised with the suite green.
	probe, err := loadablePE(t, nil)
	if err != nil {
		t.Skipf("running loader refused the synthetic image (%v); skipping the mapped-marker cases (trendvidia/go#48)", err)
	}
	syscall.FreeLibrary(probe)

	cases := []struct {
		name    string
		mutate  func(p *peImage)
		wantErr string // "" means the mapped image must be accepted
	}{
		{
			name: "well-formed mapped image",
		},
		{
			name: "version from another generation",
			mutate: func(p *peImage) {
				p.u32(p.markerOff+4, pluginabi.Version+1)
			},
			wantErr: "the loaded plugin module was built with an incompatible toolchain ABI",
		},
		{
			name: "corrupted marker word",
			mutate: func(p *peImage) {
				p.u32(p.markerOff, pluginabi.Marker^1)
			},
			wantErr: "unrecognised go:plugin.abi marker in the loaded module",
		},
		{
			name:    "export renamed",
			mutate:  func(p *peImage) { p.buf[p.nameOff] = 'x' },
			wantErr: "exports no go:plugin.abi marker",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h, err := loadablePE(t, tt.mutate)
			if err != nil {
				t.Fatalf("loader refused the synthetic image (%v) though the unmutated one loaded", err)
			}
			defer syscall.FreeLibrary(h)
			err = checkLoadedABIMarker(h)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("checkLoadedABIMarker = %v; want acceptance", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("checkLoadedABIMarker accepted the image; want %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("checkLoadedABIMarker = %v; want %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadedModulePath pins the two loader-identity helpers the
// post-load check is built on. Neither had a test: the only fixture
// that reaches them is the end-to-end samename host, and on a loader
// that maps same-named plugins separately (measured on Windows 11
// arm64) that host never takes the comparison branch at all.
func TestLoadedModulePath(t *testing.T) {
	// Handle 0 is the calling process's own image, so os.Executable is
	// the independent oracle — it reaches the same API through
	// internal/syscall/windows' generated wrapper, whose DWORD
	// narrowing and r < n truncation test loadedModulePath mirrors.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got := loadedModulePath(0); got != self {
		t.Errorf("loadedModulePath(0) = %q; want %q", got, self)
	}

	h, err := syscall.LoadLibrary("kernel32.dll")
	if err != nil {
		t.Skipf("kernel32 unavailable: %v", err)
	}
	defer syscall.FreeLibrary(h)
	got := loadedModulePath(h)
	if !strings.EqualFold(filepath.Base(got), "kernel32.dll") {
		t.Errorf("loadedModulePath(kernel32) = %q; want a path whose base is kernel32.dll", got)
	}
	if !sameModulePath(got, got) {
		t.Errorf("sameModulePath(%q, %q) = false; want true", got, got)
	}
}

func TestSameModulePath(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", `C:\dir\p.dll`, `C:\dir\p.dll`, true},
		{"case differs", `C:\Dir\P.DLL`, `c:\dir\p.dll`, true},
		{"forward slashes", `C:/dir/p.dll`, `C:\dir\p.dll`, true},
		{"literal device prefix", `\\?\C:\dir\p.dll`, `C:\dir\p.dll`, true},
		// The alias trendvidia/go#40 is about: one base name, two
		// directories. Rejecting this is the whole point.
		{"same base, other directory", `C:\a\p.dll`, `C:\b\p.dll`, false},
		{"different base", `C:\dir\p.dll`, `C:\dir\q.dll`, false},
		// loadedModulePath's "unknown" answer must never compare equal
		// to a real path; open guards on it separately, and this keeps
		// that guard from being the only thing standing between an
		// unanswered query and a silent match.
		{"unknown against a path", ``, `C:\dir\p.dll`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameModulePath(tt.a, tt.b); got != tt.want {
				t.Errorf("sameModulePath(%q, %q) = %v; want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
