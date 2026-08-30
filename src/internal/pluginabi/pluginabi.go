// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package pluginabi defines the cross-binary ABI marker carried by
// every Windows plugin DLL (trendvidia/go#24, part 2).
//
// The linker embeds [Marker] followed by [Version] into each
// -buildmode=plugin DLL as the go:plugin.abi export. Package plugin on
// Windows reads that pair twice: parsed from the candidate file's
// export table before LoadLibrary maps the image — so a binary whose
// marker is absent or whose Version differs never executes its DllMain
// — and re-read from the mapped module via GetProcAddress, so what is
// judged after the load is the image actually mapped, not whatever the
// file held when it was read (trendvidia/go#40). Without this fence, a
// DLL built by a different fork/toolchain generation appends its
// moduledata fine (it touches only its own image), and the fatal fault
// comes later, when the host walks that foreign memory at its own
// field offsets.
//
// This is the decision recorded on trendvidia/go#24: no ABI pin inside
// moduledata/pcHeader; the marker's only consumer is package plugin,
// so revising it costs a revert rather than a migration.
//
// # When to bump Version
//
// Raise Version — and only then — when a change alters what
// independently built host/plugin binaries assume about each other:
//
//   - the moduledata field set, order, or size;
//   - pcHeader layout;
//   - funcval / itab / _type representation across modules;
//   - the go:link.pkghashes carrier format;
//   - anything else consumed by runtime.addmoduledata,
//     plugin_lastmoduleinit, or the cross-module canonicalisation
//     passes (canonicalSyms, userDataInit, funcptrs, itabptrs).
//
// Bug fixes that preserve every cross-binary assumption do not bump.
package pluginabi

// Marker identifies the export as this fork's ABI marker rather than a
// coincidence of bytes. It doubles as the byte locator the test suite
// uses to find (and tamper with) the Version word in fixture DLLs.
const Marker uint32 = 0x41424950 // "ABIP" little-endian

// Version is the current cross-binary ABI generation of the fork's
// plugin machinery. See the package doc comment for bump rules.
//
// History:
//
//	2: moduledata grew the udaddrwords/udaddrslots/udaddradds triplet
//	   (static-data address canonicalisation, trendvidia/go#25).
//	1: initial marker generation (trendvidia/go#24).
const Version uint32 = 2
