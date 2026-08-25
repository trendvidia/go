// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package plugin

import (
	"internal/pluginabi"
	"os"
	"path/filepath"
	"strings"
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
	bssRVA    = 0x3000
	imageSize = 0x600
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
