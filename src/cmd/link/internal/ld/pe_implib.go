// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bufio"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cmd/internal/sys"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
)

// peSkipSymName reports whether the given symbol name should be excluded
// from PE export tables and import libraries. It filters out internal
// linker symbols, PE artifacts, and type descriptors.
func peSkipSymName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, "__") {
		return true
	}
	if strings.HasPrefix(name, "type:") || strings.HasPrefix(name, "type..") {
		return true
	}
	return false
}

// peSectionMarkerSym reports whether name is a linker-defined section
// boundary symbol. These mark the start/end of PE sections in the
// moduledata and must never be imported from the host — doing so would
// make the plugin's moduledata point to the HOST's section bounds
// instead of the DLL's, causing the GC to crash.
var peSectionMarkers = map[string]bool{
	"runtime.text":       true,
	"runtime.etext":      true,
	"runtime.types":      true,
	"runtime.etypes":     true,
	"runtime.rodata":     true,
	"runtime.erodata":    true,
	"runtime.noptrdata":  true,
	"runtime.enoptrdata": true,
	"runtime.data":       true,
	"runtime.edata":      true,
	"runtime.bss":        true,
	"runtime.ebss":       true,
	"runtime.noptrbss":   true,
	"runtime.enoptrbss":  true,
	"runtime.end":        true,
	"runtime.gcdata":     true,
	"runtime.gcbss":      true,
	"runtime.covctrs":    true,
	"runtime.ecovctrs":   true,
}

// impLibSymbol holds info about a symbol to include in the import library.
type impLibSymbol struct {
	name   string
	isFunc bool
}

// peGenerateImportLibrary creates a MinGW-compatible import library (.a)
// for the current executable. This allows plugin DLLs to link against the
// host's exported symbols without requiring an external linker (cgo) for
// the host build.
//
// The import library uses the "short import" object format as described in
// the PE/COFF specification (Import Library Format).
//
// This function collects exportable symbols directly from the loader,
// independent of the dexport slice used by the PE export table.
func peGenerateImportLibrary(ctxt *Link) {
	implib := *flagOutfile + ".a"
	// Use the final executable name (not the temp build path) for the
	// DLL name in the import library. This ensures the plugin DLL
	// imports from "host.exe" instead of "a.out.exe".
	dllName := filepath.Base(*flagOutfile)
	if *flagPluginExeName != "" {
		dllName = *flagPluginExeName
	}

	var machine uint16
	switch ctxt.Arch.Family {
	case sys.AMD64:
		machine = pe.IMAGE_FILE_MACHINE_AMD64
	case sys.I386:
		machine = pe.IMAGE_FILE_MACHINE_I386
	case sys.ARM64:
		machine = pe.IMAGE_FILE_MACHINE_ARM64
	default:
		Errorf("unsupported architecture for import library generation")
		return
	}

	ldr := ctxt.loader

	// Collect exportable symbols from the loader. Only include reachable
	// symbols — the host's PE export table only contains reachable symbols,
	// so the import library must match. Critical symbols like
	// runtime.addmoduledata are made reachable by the deadcode pass
	// (which marks all defined symbols reachable for plugin hosts).
	// Collect unique symbols by ext name. When both ABI0 and ABIInternal
	// versions exist for a function, we only need one import library entry
	// (PE imports are by name, not ABI version).
	seen := make(map[string]bool)
	var symbols []impLibSymbol
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		st := ldr.SymType(s)
		if !st.IsText() && !st.IsRODATA() && !st.IsData() {
			continue
		}
		name := ldr.SymExtname(s)
		if peSkipSymName(name) || seen[name] {
			continue
		}
		seen[name] = true
		isFunc := st.IsText()
		symbols = append(symbols, impLibSymbol{name: name, isFunc: isFunc})
		// PE export ordinals are uint16; cap at 65535.
		if len(symbols) >= 0xFFFF {
			break
		}
	}

	if len(symbols) == 0 {
		return
	}

	// Sort alphabetically (required by PE import library convention).
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].name < symbols[j].name
	})

	f, err := os.Create(implib)
	if err != nil {
		Exitf("cannot create import library %s: %v", implib, err)
	}

	w := bufio.NewWriter(f)

	// Write ar archive header.
	w.WriteString("!<arch>\n")

	// Write first linker member (symbol directory).
	writeFirstLinkerMember(w, symbols, dllName, machine)

	// Write individual short import objects.
	for i, sym := range symbols {
		writeShortImport(w, sym.name, dllName, machine, sym.isFunc, uint16(i))
	}

	if err := w.Flush(); err != nil {
		Exitf("error writing import library %s: %v", implib, err)
	}
	if err := f.Close(); err != nil {
		Exitf("error closing import library %s: %v", implib, err)
	}
}

// writeFirstLinkerMember writes the "/" archive member that serves as the
// symbol directory for the import library.
func writeFirstLinkerMember(f io.Writer, symbols []impLibSymbol, dllName string, machine uint16) {
	numSyms := len(symbols)

	// Calculate the offset where each import object will start.
	// Size of linker member data: 4 + numSyms*4 + sum(len(name)+1)
	var namesSize int
	for _, sym := range symbols {
		namesSize += len(sym.name) + 1
	}
	linkerDataSize := 4 + numSyms*4 + namesSize

	// Pad to even boundary if needed for ar header calculation.
	linkerMemberSize := linkerDataSize
	linkerPad := linkerMemberSize % 2

	// The first import object starts after: ar_magic(8) + linkerMember_arHeader(60) + linkerData + pad
	firstObjOffset := 8 + 60 + linkerMemberSize + linkerPad

	// Calculate offset for each import object.
	offsets := make([]uint32, numSyms)
	currentOffset := firstObjOffset
	for i, sym := range symbols {
		offsets[i] = uint32(currentOffset)
		objSize := shortImportSize(sym.name, dllName)
		currentOffset += 60 + objSize // 60 = ar header size
		if objSize%2 != 0 {
			currentOffset++ // ar padding
		}
	}

	// Write ar header for linker member.
	writeArHeader(f, "/", linkerMemberSize)

	// Write number of symbols (big-endian).
	binary.Write(f, binary.BigEndian, uint32(numSyms))

	// Write offsets (big-endian).
	for _, off := range offsets {
		binary.Write(f, binary.BigEndian, off)
	}

	// Write symbol names (null-terminated).
	for _, sym := range symbols {
		io.WriteString(f, sym.name)
		f.Write([]byte{0})
	}

	// Pad to even boundary.
	if linkerPad != 0 {
		f.Write([]byte{'\n'})
	}
}

// shortImportSize returns the data size of a short import object.
func shortImportSize(symName, dllName string) int {
	// Short import header (20 bytes) + symbol name + \0 + dll name + \0
	return 20 + len(symName) + 1 + len(dllName) + 1
}

// writeShortImport writes one short import object as an ar member.
// This uses the IMPORT_OBJECT_HEADER format from the PE/COFF spec.
func writeShortImport(f io.Writer, symName, dllName string, machine uint16, isFunc bool, hint uint16) {
	dataSize := len(symName) + 1 + len(dllName) + 1
	totalSize := 20 + dataSize // 20 = sizeof(IMPORT_OBJECT_HEADER)

	// Use symbol name as ar member name, truncated to 15 chars.
	arName := symName
	if len(arName) > 15 {
		arName = arName[:15]
	}

	writeArHeader(f, arName, totalSize)

	// IMPORT_OBJECT_HEADER (20 bytes):
	var hdr [20]byte
	// Sig1 = 0x0000 (IMAGE_FILE_MACHINE_UNKNOWN)
	binary.LittleEndian.PutUint16(hdr[0:2], 0x0000)
	// Sig2 = 0xFFFF
	binary.LittleEndian.PutUint16(hdr[2:4], 0xFFFF)
	// Version = 0
	binary.LittleEndian.PutUint16(hdr[4:6], 0)
	// Machine
	binary.LittleEndian.PutUint16(hdr[6:8], machine)
	// TimeDateStamp = 0
	binary.LittleEndian.PutUint32(hdr[8:12], 0)
	// SizeOfData = size of strings after header
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(dataSize))
	// OrdinalHint
	binary.LittleEndian.PutUint16(hdr[16:18], hint)
	// Type (2 bits) | NameType (3 bits) | Reserved (11 bits)
	// Type: IMPORT_OBJECT_CODE=0, IMPORT_OBJECT_DATA=1
	// NameType: IMPORT_OBJECT_NAME=1 (name matches symbol)
	var typeField uint16
	if isFunc {
		typeField = 0 // IMPORT_OBJECT_CODE
	} else {
		typeField = 1 // IMPORT_OBJECT_DATA
	}
	typeField |= (1 << 2) // NameType = IMPORT_OBJECT_NAME (bits 2-4)
	binary.LittleEndian.PutUint16(hdr[18:20], typeField)

	f.Write(hdr[:])

	// Symbol name (null-terminated)
	io.WriteString(f, symName)
	f.Write([]byte{0})

	// DLL name (null-terminated)
	io.WriteString(f, dllName)
	f.Write([]byte{0})

	// Pad to even boundary.
	if totalSize%2 != 0 {
		f.Write([]byte{'\n'})
	}
}

// writeArHeader writes a 60-byte ar archive member header.
func writeArHeader(f io.Writer, name string, size int) {
	// Format: name/16 + mtime/12 + uid/6 + gid/6 + mode/8 + size/10 + magic/2
	var hdr [60]byte

	// Name field (16 bytes, space-padded).
	copy(hdr[0:16], fmt.Sprintf("%-16s", name+"/"))
	// Timestamp (12 bytes).
	copy(hdr[16:28], "0           ")
	// UID (6 bytes).
	copy(hdr[28:34], "0     ")
	// GID (6 bytes).
	copy(hdr[34:40], "0     ")
	// Mode (8 bytes).
	copy(hdr[40:48], "100666  ")
	// Size (10 bytes).
	copy(hdr[48:58], fmt.Sprintf("%-10d", size))
	// Magic.
	hdr[58] = '`'
	hdr[59] = '\n'

	f.Write(hdr[:])
}

// peLoadImportLibrary reads a MinGW-compatible import library (.a) and
// registers all symbols in it as dynamic imports from the specified DLL/exe.
// This allows the internal linker to produce a plugin DLL that imports
// runtime symbols from the host executable, without needing an external linker.
func peLoadImportLibrary(ctxt *Link, implib string) {
	// Resolve relative paths since the linker may run from a different directory.
	if !filepath.IsAbs(implib) {
		if abs, err := filepath.Abs(implib); err == nil {
			implib = abs
		}
	}

	f, err := os.Open(implib)
	if err != nil {
		Exitf("cannot open import library %s: %v", implib, err)
	}
	defer f.Close()

	// Read and verify ar magic.
	var magic [8]byte
	if _, err := f.Read(magic[:]); err != nil || string(magic[:]) != "!<arch>\n" {
		Exitf("%s: not a valid ar archive", implib)
	}

	ldr := ctxt.loader
	importCount := 0

	// Parse ar members looking for short import objects.
	for {
		// Read ar header (60 bytes).
		var arHdr [60]byte
		if _, err := f.Read(arHdr[:]); err != nil {
			break // EOF
		}

		// Parse size from ar header (bytes 48-57).
		sizeStr := strings.TrimSpace(string(arHdr[48:58]))
		var size int
		if _, err := fmt.Sscanf(sizeStr, "%d", &size); err != nil || size <= 0 {
			break
		}

		// Read member data.
		data := make([]byte, size)
		if _, err := f.Read(data); err != nil {
			break
		}

		// Skip padding byte for odd-sized members.
		if size%2 != 0 {
			f.Read(make([]byte, 1))
		}

		// Check for short import object signature:
		// Sig1 = 0x0000, Sig2 = 0xFFFF
		if len(data) < 20 {
			continue
		}
		sig1 := binary.LittleEndian.Uint16(data[0:2])
		sig2 := binary.LittleEndian.Uint16(data[2:4])
		if sig1 != 0x0000 || sig2 != 0xFFFF {
			continue // Not a short import object (e.g., linker member)
		}

		// Parse the short import object.
		// After the 20-byte header: null-terminated symbol name, then null-terminated DLL name.
		strData := data[20:]
		nullIdx := bytes.IndexByte(strData, 0)
		if nullIdx < 0 {
			continue
		}
		symName := string(strData[:nullIdx])
		strData = strData[nullIdx+1:]
		nullIdx = bytes.IndexByte(strData, 0)
		if nullIdx < 0 {
			continue
		}
		dllName := string(strData[:nullIdx])

		if symName == "" || dllName == "" {
			continue
		}

		if peSkipSymName(symName) {
			continue
		}
		// Skip function value descriptors (·f symbols). These are DATA
		// containing function pointers. They must stay local so type
		// descriptors can read from them. Their R_ADDR to the actual
		// function will resolve to the PLT stub address.
		if strings.HasSuffix(symName, "\u00b7f") {
			continue
		}
		// Skip linker-defined section boundary markers. These must
		// remain local so the plugin's moduledata points to the DLL's
		// own sections, not the host's.
		if peSectionMarkers[symName] {
			continue
		}

		// Convert to SDYNIMPORT. Only convert TEXT (function) symbols.
		// On amd64, DATA symbols cannot be imported via IAT because the
		// compiler generates direct memory access (LEA/MOV [rip+disp32])
		// with no indirection. Unlike arm64 (which has ADRP+ADD→LDR
		// rewriting for SWINDOWS data), amd64 has no equivalent.
		// DATA symbols remain as local copies in the plugin DLL.
		//
		// Must convert BOTH ABI versions (ABI0 and ABIInternal) for
		// functions.
		found := false
		for _, ver := range []int{0, 1} {
			s := ldr.Lookup(symName, ver)
			if s == 0 {
				continue
			}
			st := ldr.SymType(s)
			if st == sym.SDYNIMPORT {
				continue // Already an import (e.g., Windows API)
			}
			// Skip DATA/RODATA — no IAT indirection on amd64.
			// Exceptions:
			// - ..inittask: must be converted to prevent double-init
			// - runtime.writeBarrier: must be shared so the plugin's
			//   inline write barrier check sees the host's GC state.
			//   The CMPL reads the IAT pointer (always non-zero), so
			//   write barriers are always "enabled" in plugin code.
			//   The host's gcWriteBarrier checks the real state and
			//   handles it correctly. Without this, parallel GC crashes
			//   because the plugin never executes write barriers.
			if !st.IsText() &&
				!strings.HasSuffix(symName, "..inittask") &&
				symName != "runtime.writeBarrier" {
				continue
			}
			sb := ldr.MakeSymbolUpdater(s)
			sb.SetType(sym.SDYNIMPORT)
			sb.SetSize(0)
			sb.SetData(nil)
			sb.ResetRelocs()
			ldr.FreeData(s)
			ldr.SetSymDynimplib(s, dllName)
			ldr.SetSymExtname(s, symName)
			// Mark reachable so initdynimport creates IAT entries.
			ldr.SetAttrReachable(s, true)
			importCount++
			found = true
		}
		if !found {
			continue // Not referenced by plugin
		}
	}
	if ctxt.Debugvlog > 0 {
		ctxt.Logf("plugin: loaded %d import symbols from %s\n", importCount, implib)
	}
}

// filterConvertedSyms removes SDYNIMPORT and SWINDOWS symbols from Textp.
// These are host-provided symbols that were converted from archive defs.
func filterConvertedSyms(ctxt *Link) []loader.Sym {
	ldr := ctxt.loader
	j := 0
	for _, s := range ctxt.Textp {
		st := ldr.SymType(s)
		if st != sym.SDYNIMPORT && st != sym.SWINDOWS {
			ctxt.Textp[j] = s
			j++
		}
	}
	return ctxt.Textp[:j]
}

// peExportAllSymbols marks all reachable text/data symbols for export in
// the PE export table. This is the internal-linker equivalent of MinGW's
// --export-all-symbols flag.
func peExportAllSymbols(ctxt *Link) {
	ldr := ctxt.loader
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !ldr.AttrReachable(s) {
			continue
		}
		st := ldr.SymType(s)
		if !st.IsText() && !st.IsRODATA() && !st.IsData() {
			continue
		}
		name := ldr.SymExtname(s)
		if peSkipSymName(name) {
			continue
		}
		// Skip symbols that are already in the export list.
		if ldr.AttrCgoExportDynamic(s) {
			continue
		}
		ldr.SetAttrCgoExportDynamic(s, true)
	}
}

