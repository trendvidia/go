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
	"unsafe"
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
// exactly one caller — the probe in TestCheckLoadedABIMarker — judges
// whether the synthetic shape is loadable at all, and fails the test
// if not: the shape is measured loadable on windows/amd64 and
// windows/arm64, so a refusal is a regression in this fixture or a
// loader rule worth knowing about, never a reason to leave the
// accept/reject matrix unexercised with the suite green.
func loadablePE(t *testing.T, mutate func(p *peImage)) (syscall.Handle, error) {
	t.Helper()
	return syscall.LoadLibrary(writeLoadablePE(t, mutate))
}

// writeLoadablePE builds the loadablePE image and writes it to a fresh
// .dll path, returning the path so a test can hand it to open itself:
// open takes a path, not a handle, and the image is exactly the input
// of its chain-did-not-grow branch (a loadable DLL exporting a valid
// marker that never chains any moduledata).
func writeLoadablePE(t *testing.T, mutate func(p *peImage)) string {
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
	// The three settings below are what the windows/arm64 loader
	// demands beyond amd64's (measured on Windows 11 arm64, 2026-08-27,
	// by mutating a clang-linked DLL that loads until it stopped: an
	// ImageBase below 4 GiB or DllCharacteristics without both
	// DYNAMIC_BASE and NX_COMPAT answers "%1 is not a valid Win32
	// application"; amd64 accepts every combination). The relocation
	// directory is a terminating empty block so a DYNAMIC_BASE image
	// whose preferred base is taken relocates instead of failing.
	p.u32(p.optOff+24, 0x80000000)         // ImageBase = 0x1_8000_0000, the default 64-bit DLL base
	p.u32(p.optOff+28, 0x1)                // ... high dword
	p.u16(p.optOff+70, 0x160)              // DllCharacteristics: HIGH_ENTROPY_VA | DYNAMIC_BASE | NX_COMPAT
	p.u32(p.optOff+112+5*8, rdataRVA+0x40) // data directory[5]: base relocation table
	p.u32(p.optOff+116+5*8, 8)             // ... one empty block
	p.u32(rdataPtr+0x40, textRVA)          // IMAGE_BASE_RELOCATION.VirtualAddress
	p.u32(rdataPtr+0x44, 8)                // IMAGE_BASE_RELOCATION.SizeOfBlock (header only)
	p.u32(p.optOff+32, 0x1000)             // SectionAlignment
	p.u32(p.optOff+36, 0x200)              // FileAlignment
	p.u16(p.optOff+40, 6)                  // MajorOperatingSystemVersion (6.0 = Vista, under any host of this fork)
	p.u16(p.optOff+48, 6)                  // MajorSubsystemVersion
	p.u32(p.optOff+56, 0x4000)             // SizeOfImage (covers .text ending at RVA 0x3200, multiple of SectionAlignment)
	p.u32(p.optOff+60, 0x200)              // SizeOfHeaders
	p.u16(p.optOff+68, 3)                  // Subsystem: IMAGE_SUBSYSTEM_WINDOWS_CUI
	p.u32(p.optOff+108, 16)                // NumberOfRvaAndSizes (the export directory lives in slot 0)
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
	return path
}

var procGetModuleHandleW = modkernel32.NewProc("GetModuleHandleW")

// TestOpenNoModuleData drives open through its chain-did-not-grow
// branch, which no end-to-end fixture reaches: every real plugin this
// toolchain links chains its moduledata from DllMain, and the samename
// host cannot alias on a loader that maps same-named DLLs separately
// (measured on Windows 11 arm64). The loadablePE image is precisely
// this branch's input — it passes stage 1, LoadLibrary maps it, and
// nothing is appended to the chain — so open must report the
// no-module-data diagnosis (not the alias one: the loader's recorded
// path and the request name the same file, whatever spelling
// t.TempDir carries), cache that failure, and FreeLibrary the image.
func TestOpenNoModuleData(t *testing.T) {
	path := writeLoadablePE(t, nil)
	const noModData = "registered no Go module data in this process"

	p, err := open(path)
	if err == nil {
		p.Close()
		t.Fatalf("open(%q) succeeded; want the no-module-data rejection", path)
	}
	if !strings.Contains(err.Error(), noModData) {
		t.Fatalf("open(%q) = %v; want an error containing %q", path, err, noModData)
	}
	if strings.Contains(err.Error(), "resolved to a different module") {
		t.Fatalf("open(%q) = %v; a fresh map was misdiagnosed as an alias", path, err)
	}

	// The failure is cached: a second Open must not map the file again.
	if _, err := open(path); err == nil || !strings.Contains(err.Error(), "(previous failure)") {
		t.Fatalf("second open(%q) = %v; want the cached %q", path, err, "(previous failure)")
	}

	// The branch FreeLibrary'd the only reference it took, so the loader
	// no longer knows the module.
	up, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, _ := procGetModuleHandleW.Call(uintptr(unsafe.Pointer(up))); h != 0 {
		t.Errorf("GetModuleHandleW(%q) = %#x after the rejection; want 0 (image unmapped)", path, h)
	}
}

// TestOpenShortNameCacheKey pins that the plugins cache is keyed by
// the file's identity and not by its spelling: an image already opened
// through its long path must be FOUND by a second open through the 8.3
// short spelling, which then reports the cached outcome instead of
// going back to the loader.
//
// It matters because the loader hands a second LoadLibrary of an
// already-mapped module the existing handle without re-running
// DllMain, so a spelling that misses the cache lands in the
// chain-did-not-grow branch — which cannot tell that case from a fresh
// map registering no module data, and so caches a flatly false
// diagnosis under the second spelling for the life of the process.
// This fixture reaches the cache lookup, not that loader state: its
// image never chains, so the first open frees it and a missed cache
// would genuinely re-map. The distinguishing assertion is therefore
// the cached "(previous failure)", which is exactly the lookup the
// aliasing case depends on.
func TestOpenShortNameCacheKey(t *testing.T) {
	path := writeLoadablePE(t, nil)
	short := shortPathName(t, path)
	if strings.EqualFold(short, path) {
		t.Skip("volume generates no 8.3 short names")
	}

	if _, err := open(path); err == nil || !strings.Contains(err.Error(), "registered no Go module data in this process") {
		t.Fatalf("open(%q) = %v; want the no-module-data rejection", path, err)
	}
	if _, err := open(short); err == nil || !strings.Contains(err.Error(), "(previous failure)") {
		t.Fatalf("open(%q) = %v; want the failure cached under the long spelling %q", short, err, path)
	}
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
	// One probe judges the whole value-level set: the running loader
	// must accept the synthetic shape, and then every case below must
	// load. The shape is measured loadable on both Windows
	// architectures this fork supports (see loadablePE), so a refusal
	// here is a failure and not a skip — skipping used to leave
	// checkLoadedABIMarker's accept/reject matrix entirely unexercised
	// on windows/arm64 with the suite green (trendvidia/go-archive#48).
	probe, err := loadablePE(t, nil)
	if err != nil {
		t.Fatalf("running loader refused the synthetic image: %v", err)
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

	// The unknown answer, which open() reads as "do not reject": a
	// handle that names no module. 1 is not page-aligned, so it cannot
	// be any image's base address, and GetModuleFileNameW answers 0
	// (ERROR_MOD_NOT_FOUND) rather than filling the buffer. This is the
	// return that makes the alias comparison stand down instead of
	// refusing a load, so it is worth pinning that it is produced —
	// quietly, without the panic LazyProc.Call raises for a missing
	// procedure while open holds pluginsMu.
	if got := loadedModulePath(syscall.Handle(1)); got != "" {
		t.Errorf("loadedModulePath(1) = %q; want %q for a handle that names no module", got, "")
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
		// The alias trendvidia/go-archive#40 is about: one base name, two
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

	// An 8.3 short name spells the same file the loader records under
	// its long name; open compares the two after a load through the
	// short spelling, and a mismatch there would misreport an ordinary
	// load as an alias. Real files are needed: the expansion is the
	// filesystem's, not a string rule.
	t.Run("short vs long name", func(t *testing.T) {
		// Expand the temp root before building the fixture: a test's
		// "long" path is only as long as its base, and %TMP% is not
		// guaranteed to be spelled out. The self-hosted windows/arm64
		// runner runs as NetworkService, whose temp directory is
		// C:\WINDOWS\SERVIC~1\NETWOR~1\AppData\Local\Temp — so an
		// unexpanded base leaves short-name components INSIDE long,
		// and longPathName, which correctly expands all of them, is
		// then measured against a target that was never fully long.
		base := longPathName(t.TempDir())
		dir := filepath.Join(base, "a directory name well past eight chars")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		long := filepath.Join(dir, "a-plugin-name-well-past-eight-chars.dll")
		other := filepath.Join(dir, "another-plugin-name-past-eight-chars.dll")
		for _, f := range []string{long, other} {
			if err := os.WriteFile(f, nil, 0o666); err != nil {
				t.Fatal(err)
			}
		}
		short := shortPathName(t, long)
		if strings.EqualFold(short, long) {
			t.Skip("volume generates no 8.3 short names")
		}
		if !sameModulePath(long, short) || !sameModulePath(short, long) {
			t.Errorf("sameModulePath(%q, %q) = false; want true for one file's short and long names", long, short)
		}
		if sameModulePath(short, other) {
			t.Errorf("sameModulePath(%q, %q) = true; want false for different files", short, other)
		}
		if got := longPathName(short); !strings.EqualFold(got, long) {
			t.Errorf("longPathName(%q) = %q; want %q", short, got, long)
		}
		if got := longPathName(`C:\no\such\dir\p.dll`); got != `C:\no\such\dir\p.dll` {
			t.Errorf("longPathName(missing) = %q; want the input unchanged", got)
		}
	})
}

// shortPathName returns the 8.3 spelling of path, or path itself when
// the volume keeps no short names.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	up, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Grow the buffer on truncation, the idiom loadedModulePath and
	// longPathName both use: GetShortPathNameW answers a too-small
	// buffer by returning the size the result NEEDS, with no error, so
	// a fixed buffer would slice out of range and panic instead of
	// failing this test with a diagnosis.
	for size := uint32(syscall.MAX_PATH); size <= 65536; size *= 2 {
		buf := make([]uint16, size)
		n, err := syscall.GetShortPathName(up, &buf[0], size)
		if err != nil {
			t.Fatalf("GetShortPathName(%q): %v", path, err)
		}
		if n >= size {
			continue
		}
		return syscall.UTF16ToString(buf[:n])
	}
	t.Fatalf("GetShortPathName(%q): no answer fits 64Ki UTF-16 units", path)
	return ""
}
