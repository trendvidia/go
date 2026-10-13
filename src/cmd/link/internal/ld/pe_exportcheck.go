// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"

	"cmd/internal/objabi"
)

// peImageExports is what a linked PE image's export directory publishes.
type peImageExports struct {
	// names holds the exported names, in export-name-table order. It is
	// IMAGE_EXPORT_DIRECTORY.NumberOfNames entries long.
	names []string

	// nFunctions is IMAGE_EXPORT_DIRECTORY.NumberOfFunctions, the size of
	// the export address table. Every entry there consumes an ordinal
	// whether or not a name points at it, so this — not len(names) — is
	// the number peExportLimit bounds. NumberOfNames <= NumberOfFunctions
	// always, and they are equal only when every export is named.
	nFunctions int
}

// peImageExportNames returns the names in a linked PE image's export
// directory, in export-table order.
func peImageExportNames(path string) ([]string, error) {
	ex, err := peImageExportDir(path)
	return ex.names, err
}

// peImageExportDir reads a linked PE image's export directory.
//
// This reads the finished image rather than any list the linker built,
// which is the point: on the external-link path the image is produced by
// mingw ld or lld-link, and what it publishes is not what we handed it.
// See peVerifyImageExportCount.
//
// An image with no export directory at all is not an error: it reports
// zero of everything, and it is the caller that decides whether zero is a
// legitimate answer for the image it asked about.
func peImageExportDir(path string) (peImageExports, error) {
	var ex peImageExports
	f, err := pe.Open(path)
	if err != nil {
		return ex, err
	}
	defer f.Close()

	var dd []pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		dd = oh.DataDirectory[:]
	case *pe.OptionalHeader32:
		dd = oh.DataDirectory[:]
	default:
		return ex, fmt.Errorf("%s: no optional header", path)
	}
	ed := dd[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
	if ed.Size == 0 {
		return ex, nil
	}

	var sect *pe.Section
	for _, s := range f.Sections {
		if s.VirtualAddress <= ed.VirtualAddress && ed.VirtualAddress < s.VirtualAddress+s.VirtualSize {
			sect = s
			break
		}
	}
	if sect == nil {
		return ex, fmt.Errorf("%s: export directory RVA %#x is in no section", path, ed.VirtualAddress)
	}
	data, err := sect.Data()
	if err != nil {
		return ex, err
	}
	at := func(rva uint32) ([]byte, error) {
		off := rva - sect.VirtualAddress
		if off >= uint32(len(data)) {
			return nil, fmt.Errorf("%s: RVA %#x outside %s", path, rva, sect.Name)
		}
		return data[off:], nil
	}

	hdr, err := at(ed.VirtualAddress)
	if err != nil {
		return ex, err
	}
	if len(hdr) < 36 {
		return ex, fmt.Errorf("%s: export directory truncated", path)
	}
	ex.nFunctions = int(binary.LittleEndian.Uint32(hdr[20:]))
	nNames := binary.LittleEndian.Uint32(hdr[24:])
	namePtrs, err := at(binary.LittleEndian.Uint32(hdr[32:]))
	if err != nil {
		return ex, err
	}
	if uint64(nNames)*4 > uint64(len(namePtrs)) {
		return ex, fmt.Errorf("%s: export name table truncated (%d names)", path, nNames)
	}
	ex.names = make([]string, 0, nNames)
	for i := uint32(0); i < nNames; i++ {
		s, err := at(binary.LittleEndian.Uint32(namePtrs[i*4:]))
		if err != nil {
			return ex, err
		}
		z := bytes.IndexByte(s, 0)
		if z < 0 {
			return ex, fmt.Errorf("%s: unterminated export name", path)
		}
		ex.names = append(ex.names, string(s[:z]))
	}
	return ex, nil
}

// peVerifyImageExportCount enforces peExportLimit against what the linked
// image actually publishes. Called after hostlink, for Windows plugins
// built with an external linker.
//
// The check inside pePluginExportFile cannot do this job (trendvidia/go-archive#59).
// It counts the names written into the .def, and handing mingw ld an EXPORTS
// list does not switch its auto-export off: measured on windows_omen at
// 60fd56dbc4, a plugin whose .def named 6 symbols published 2195. So that
// check tested 6 against the limit while the image carried 366x more — a
// cap that could only ever produce false confidence, which is worse than no
// cap at all.
//
// Reading the finished file is the only measurement that covers the real
// number, because the linker that decided it is not ours. It costs one open
// and one section read of an image already on disk, and it reports after the
// link rather than before — the price of measuring the true thing instead of
// a proxy for it.
//
// Making the .def authoritative (-Wl,--exclude-all-symbols) would make the
// cheap check correct. It was tried and rejected once because it stripped
// x_cgo_notify_runtime_init_done, which plugin.Open reaches by
// GetProcAddress and which was present only because mingw auto-exported
// it; losing it hung every cgo plugin in _cgo_wait_runtime_init_done with
// no diagnostic (trendvidia/go-archive#62).
//
// That objection no longer holds: addCgoNotifyExport names the symbol in
// the .def itself, and TestCgoPluginNotifyExport builds its plugin with
// -Wl,--exclude-all-symbols precisely to hold that. Whether the cheap
// check is now worth having in place of this one is a live question and
// nobody has measured it, so this stays as it is rather than being
// changed on the strength of a comment.
func peVerifyImageExportCount(ctxt *Link) {
	if ctxt.HeadType != objabi.Hwindows || ctxt.BuildMode != BuildModePlugin {
		return
	}
	if ctxt.LinkMode != LinkExternal {
		// The internal path emits the table itself, so initdynexport
		// already counted exactly what it wrote.
		return
	}
	if nerrors > 0 {
		// hostlink returns without running the external linker as soon
		// as anything has logged an error (lib.go), so *flagOutfile
		// holds no image to measure. Reporting "cannot verify" here
		// would blame the export check for a failure that happened
		// somewhere else entirely.
		return
	}
	n, over, err := peImageExportOverLimit(*flagOutfile, peExportLimit)
	if err != nil {
		// A warning, not Errorf, and that is the whole point: Errorf
		// deletes the output file and exits 2 (util.go's "Logging an
		// error means that on exit cmd/link will delete any output file
		// and return a non-zero error code"). A cap check must not be
		// the thing that breaks a link that otherwise succeeded, so a
		// parse failure here says why it could not measure and lets the
		// image stand -- a silent pass is never mistaken for a verified
		// one, but it is also never turned into a failed build.
		fmt.Fprintf(os.Stderr, "link: warning: %s: cannot verify PE export count: %v\n",
			peOutputDisplayName(), err)
		return
	}
	if n == 0 {
		// An empty export directory is not a plugin: the host resolves
		// everything in it through GetProcAddress. Zero sails under any
		// limit, so logging it as a pass would be the same false
		// confidence trendvidia/go-archive#59 was filed about. Warn rather than fail, for
		// the same reason as above: the cap is what this function owns,
		// and rejecting images on a condition never measured is not.
		fmt.Fprintf(os.Stderr, "link: warning: %s publishes no exports; its export count was not verified against the %d-symbol limit\n",
			peOutputDisplayName(), peExportLimit)
		return
	}
	if over {
		Errorf("link: %s publishes %d symbols, more than the %d a PE image can address; reduce the exported surface",
			peOutputDisplayName(), n, peExportLimit)
		errorexit()
	}
	if ctxt.Debugvlog != 0 {
		ctxt.Logf("peVerifyImageExportCount: %s publishes %d of %d permitted exports\n",
			peOutputDisplayName(), n, peExportLimit)
	}
}

// peImageExportOverLimit reports how many symbols the image at path
// publishes and whether that exceeds limit. Split out from
// peVerifyImageExportCount so a test can drive it with a small limit
// against a real image — the property worth pinning is that the number
// compared comes from the linked file, and a test that could only use
// the real 65500 could never demonstrate that.
//
// The number is the size of the export address table, not the length of
// the name table. peExportLimit exists because "PE export ordinals are
// uint16" (initdynexport), and every address-table entry burns an ordinal
// whether a name points at it or not: a NONAME or ordinal-only export
// makes NumberOfNames the smaller of the two, and counting names would
// then undercount exactly the way trendvidia/go-archive#59's .def count did. The larger of the
// two is taken so a malformed directory cannot shrink the number either.
//
// Ordinal Base is not modelled: every linker in play emits 1, and the
// limit already leaves 35 ordinals of slack under the uint16 ceiling.
func peImageExportOverLimit(path string, limit int) (n int, over bool, err error) {
	ex, err := peImageExportDir(path)
	if err != nil {
		return 0, false, err
	}
	n = ex.nFunctions
	if len(ex.names) > n {
		n = len(ex.names)
	}
	return n, n > limit, nil
}
