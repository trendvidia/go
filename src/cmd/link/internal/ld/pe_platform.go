// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The Windows/PE half of the cmd/link platform split.
//
// Upstream inlines `if ctxt.HeadType == objabi.Hwindows` branches into the
// shared paths in lib.go, data.go and dwarf.go. The fork calls out to the
// helpers here instead, so the Windows specifics sit in one place and the
// shared files stay readable. Nothing here is plugin-specific; the plugin
// export boundary is in pe_plugin.go.
//
// They live apart from pe.go, which is upstream's PE writer, so that upstream
// changes to it merge without touching fork-authored code. See
// trendvidia/go#140.

package ld

import (
	"cmd/internal/bio"
	"cmd/internal/objabi"
	"cmd/link/internal/loadpe"
	"cmd/link/internal/sym"
	"fmt"
	"strings"
)

// ldpe loads a PE host object as part of ldobj's format dispatch.
func ldpe(ctxt *Link, f *bio.Reader, pkg string, length int64, pn string) {
	ls, err := loadpe.Load(ctxt.loader, ctxt.Arch, ctxt.IncVersion(), f, pkg, length, pn)
	if err != nil {
		Errorf("%v", err)
		return
	}
	if len(ls.Resources) != 0 {
		setpersrc(ctxt, ls.Resources)
	}
	sehp.pdata = append(sehp.pdata, ls.PData...)
	if ls.XData != 0 {
		sehp.xdata = append(sehp.xdata, ls.XData)
	}
	ctxt.Textp = append(ctxt.Textp, ls.Textp...)
}

// peSegmentFileOffset returns the file offset for a segment placed after
// prev in a PE binary. PE requires file-alignment rounding on the
// previous segment's length using PEFILEALIGN, but does NOT require the
// stricter (Vaddr%R == Fileoff%R) invariant that ELF-style layouts
// enforce — the PE loader maps sections via separate VirtualAddress
// and PointerToRawData fields, so file-side and memory-side alignment
// are independent.
func peSegmentFileOffset(prev *sym.Segment) uint64 {
	return prev.Fileoff + uint64(Rnd(int64(prev.Filelen), PEFILEALIGN))
}

// peAlignDwarfSection rounds va up to PEFILEALIGN. Called inside
// data.go::address's per-DWARF-section loop so each PE DWARF section
// starts on a file-alignment boundary.
func peAlignDwarfSection(va uint64) uint64 {
	return uint64(Rnd(int64(va), PEFILEALIGN))
}

// peNeedsGdbScriptWorkaround reports whether DWARF generation should
// skip emitting the .debug_gdb_scripts section. gcc on Windows places
// .debug_gdb_scripts in the wrong location for c-archive external
// links, which prevents the program from running
// (https://golang.org/issue/20183). Non-c-archive builds avoid this
// issue via a linker script (see writeGDBLinkerScript). c-archive
// users would need to specify the linker script manually, so for UX
// we simply omit the section.
func peNeedsGdbScriptWorkaround(ctxt *Link) bool {
	return ctxt.LinkMode == LinkExternal &&
		ctxt.HeadType == objabi.Hwindows &&
		ctxt.BuildMode == BuildModeCArchive
}

// peAppendMingwLibs appends Windows-specific late-stage hostlink
// arguments: the gcc gdb-script linker workaround, the -lsynchronization
// race-detector library, the mingw libgroup, and the per-imported-DLL
// argument list. Called from hostlink AFTER all earlier flags and .o
// files have been queued, so the libraries are linked last.
//
// The gcc gdb-script workaround is only needed for non-LLD linkers
// (https://golang.org/issue/20183); isLLD comes from peArgs, which
// detects the linker once per link (go1.27 hoisted that detection out
// of this step and into ctxt.isLLD).
//
// No-op on non-Windows targets so callers can invoke unconditionally.
func peAppendMingwLibs(ctxt *Link, argv []string, isMSVC, isLLD bool) []string {
	if ctxt.HeadType != objabi.Hwindows {
		return argv
	}

	// gcc linker script workaround (golang.org/issue/20183).
	if !isLLD {
		p := writeGDBLinkerScript()
		argv = append(argv, "-Wl,-T,"+p)
	}
	if *flagRace {
		// --print-file-name doesn't reliably resolve with -msvc clang
		// for libsynchronization, so we always pass -lsynchronization.
		if isMSVC || ctxt.findLibPath("libsynchronization.a") != "libsynchronization.a" {
			argv = append(argv, "-lsynchronization")
		}
	}
	if !isMSVC {
		// libmingw32 and libmingwex have inter-dependencies; group them.
		argv = append(argv, "-Wl,--start-group", "-lmingwex", "-lmingw32", "-Wl,--end-group")
	}
	argv = append(argv, peimporteddlls()...)
	return argv
}

// peArgs appends Windows PE-specific external-linker flags to argv
// during hostlink, and returns the updated argv together with the
// linker-style detection (isMSVC, isLLD) and the -Wl flag prefix to use
// for the rest of the link. MSVC's lld-link expects single-dash options
// while mingw expects double-dash, so the caller must use the
// returned wlPrefix for any later -Wl flags it appends. isLLD is
// reported because the late-stage mingw block needs it too, and
// detecting the linker costs a subprocess.
//
// Flags appended:
//   - -mwindows / -mconsole based on the windowsgui package flag.
//   - tsaware: marks the binary as Terminal Services aware.
//   - nxcompat: enables Data Execution Prevention.
//   - --major/minor-os-version and --major/minor-subsystem-version
//     (mingw only): set PE minimum target versions.
func peArgs(ctxt *Link, argv []string) (newArgv []string, isMSVC, isLLD bool, wlPrefix string) {
	wlPrefix = "-Wl,--"
	isMSVC = ctxt.isMSVC()
	isLLD = ctxt.isLLD()
	if isMSVC {
		// MSVC lld-link only accepts one-dash options for many flags.
		// TODO: mingw clang appears to accept either; we may be able
		// to always use one dash, but legacy compilers in use today
		// haven't been verified.
		wlPrefix = "-Wl,-"
	}

	if windowsgui {
		argv = append(argv, "-mwindows")
	} else {
		argv = append(argv, "-mconsole")
	}
	argv = append(argv, wlPrefix+"tsaware")
	argv = append(argv, wlPrefix+"nxcompat")

	if !isMSVC {
		peMajorVersion := PeMinimumTargetMajorVersion
		peMinorVersion := PeMinimumTargetMinorVersion
		if peMajorVersion >= 10 && !isLLD &&
			!peHasLoadConfigDirectorySupport(ctxt.Arch, argv[0]) {
			// The external linker doesn't support wiring up
			// _load_config_used to the PE Load Configuration
			// Directory. Windows 10+ validates this directory,
			// so fall back to an older version to avoid
			// load failures.
			peMajorVersion = 6
			peMinorVersion = 1
		}
		argv = append(argv, fmt.Sprintf("-Wl,--major-os-version=%d", peMajorVersion))
		argv = append(argv, fmt.Sprintf("-Wl,--minor-os-version=%d", peMinorVersion))
		argv = append(argv, fmt.Sprintf("-Wl,--major-subsystem-version=%d", peMajorVersion))
		argv = append(argv, fmt.Sprintf("-Wl,--minor-subsystem-version=%d", peMinorVersion))
	}
	return argv, isMSVC, isLLD, wlPrefix
}

// peNoTimestampArgs appends --no-insert-timestamp to argv for Windows
// targets, suppressing generation of the PE file header timestamp so
// that linked binaries are reproducible. The flag is probed first
// because not all toolchains support it.
func peNoTimestampArgs(ctxt *Link, argv []string, altLinker string) []string {
	if !ctxt.IsWindows() {
		return argv
	}
	const noTimeStamp = "-Wl,--no-insert-timestamp"
	if linkerFlagSupported(ctxt.Arch, argv[0], altLinker, noTimeStamp) {
		argv = append(argv, noTimeStamp)
	}
	return argv
}

// stdcallDecoration returns the index of the '%' introducing name's
// stdcall decoration, or -1 if it carries none.
//
// Symbols pulled in by //go:cgo_import_dynamic spell the number of
// uintptr-sized arguments a stdcall function takes as a `%n` SUFFIX
// (`runtime._GetSystemTimeAsFileTime%1`), which windows/386's external
// link needs to build the `_name@N` decorated form. That is the whole
// convention: the decoration is at the END and is decimal digits.
//
// Matching the FIRST '%' anywhere in the name instead — as this did
// until trendvidia/go#116 — collides with objabi.PathToPrefix, which
// escapes '.' after the final slash (plus '%', '"', and every byte
// outside printable ASCII) as %xx. Every symbol of a package whose
// import path has a dot in its last element therefore carries a `%2e`:
// `gopkg.in/yaml.v2` becomes `gopkg.in/yaml%2ev2`, and a package at the
// root of a dotted module path is escaped whole (`module example.com`
// -> `example%2ecom`, trendvidia/go#110). Those names reach here as
// SDYNIMPORT the moment a Windows plugin binds one from its host's
// import library, and the old scan then failed the link outright:
// "failed to parse stdcall decoration: parsing \"2ev2.Set\": invalid
// syntax", after truncating the extname to `gopkg.in/yaml`.
//
// A trailing-digits test separates the two exactly. An escape is
// followed by hex and then more of the name, so it can only be read as
// a decoration if it ends the name AND its two hex digits are both
// decimal — %20 (space), %22 ('"'), %25 ('%'). None of those bytes is
// legal in an import path, and a symbol name never ends at the escape.
func stdcallDecoration(name string) int {
	i := strings.LastIndexByte(name, '%')
	if i < 0 || i == len(name)-1 {
		return -1
	}
	for j := i + 1; j < len(name); j++ {
		if name[j] < '0' || name[j] > '9' {
			return -1
		}
	}
	return i
}

// peLoadConfigSym is the linker's own name for the PE load config
// structure it synthesizes on an internal link.
//
// It is deliberately NOT upstream's `_load_config_used`: go1.27's
// runtime/cgo defines that name in C, and adopting the C object's symbol
// yields one the linker cannot place. See Peinit, which explains the
// 0xc0000005 that produced.
//
// It KEEPS the leading underscore, which is load-bearing rather than
// cosmetic. peSkipSymName drops `_`-prefixed names, and it gates both
// the plugin export table (peExportAllSymbols) and the import-library
// conversion (peLoadImportLibrary). A per-image PE structure must be
// neither: exporting it publishes a linker internal, and a host that
// exported one would have the plugin's copy converted into an import,
// which is precisely the unplaced-symbol state this fix exists to
// avoid. A `go:`-prefixed name was measured leaking into the export
// directory for that reason.
const peLoadConfigSym = "_go_load_config_used"
