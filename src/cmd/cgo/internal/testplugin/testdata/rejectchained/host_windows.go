// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

// Command rejectchained drives the stage-2 half of the cross-toolchain
// ABI fence end to end, and pins how its rejection treats the chained
// residue (trendvidia/go#48):
//
//  1. plugin.Open on a file whose bytes are swapped between the fence's
//     two reads. Stage 1 judges the file at the path and stage 2 judges
//     the module the loader mapped, so the only way for stage 2 to
//     reject is for those to differ. The fixture makes them differ
//     through package plugin's testHookBetweenFenceStages: stage 1 reads
//     a healthy plugin built by this toolchain, the hook then overwrites
//     the file with a copy whose marker Version word names another
//     generation, and LoadLibrary maps that copy — its DllMain chains a
//     real moduledata exactly as a swapped-in incompatible build would.
//     Open must report the stage-2 text, leave the chain tail where it
//     was, keep the mapping resident, and answer every later Open of
//     the path from the negative cache without re-reading the file.
//
//  2. rejectChainedModule on a node that is NOT host-shaped. Every
//     module this toolchain can build has this host's moduledata
//     layout, so the patched copy above still reads as a healthy module;
//     the fence's premise — that the rejected node's fields are NOT at
//     host offsets — is never true of a real fixture. This part appends
//     a PAGE_NOACCESS page to the chain in the node's place: any read of
//     the residue faults the process, so the rejection passes only if it
//     truncates by position without touching the node.
//
// The peer is opened first on purpose. The rejection truncates the
// chain at the caller's pre-load tail, so a run with an empty plugin
// chain cannot tell that apart from truncating every plugin off the
// host; only a tail that is itself a plugin's moduledata does. A
// healthy sibling then loads to prove addmoduledata's append cursor
// moved back with the tail.
//
// The harness passes the correct {Marker, Version} pair as a hex
// argument so nothing here is hardcoded against future bumps. The
// swapped module is deliberately never freed: it chained real Go module
// data into this host, and dropping its loader reference would unmap a
// chained image — exactly what the fence refuses to do. One mapping
// stays resident for the life of the process.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"unsafe"
)

//go:linkname chainTail plugin.chainTail
func chainTail() uintptr

//go:linkname chainGrew plugin.chainGrew
func chainGrew(prevTail uintptr) bool

//go:linkname rejectChainedModule plugin.rejectChainedModule
func rejectChainedModule(prevTail uintptr)

//go:linkname testHookBetweenFenceStages plugin.testHookBetweenFenceStages
var testHookBetweenFenceStages func(pluginPath string)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc    = kernel32.NewProc("VirtualAlloc")
	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

// interface{}, not any: testdata packages build with -lang=go1.16.
func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "rejectchained: "+format+"\n", args...)
	os.Exit(1)
}

// name calls a plugin's exported Name through a fresh Lookup, so a
// module quietly dropped out of activeModules shows up here rather
// than being masked by a cached func value.
func name(p *plugin.Plugin, what string) string {
	f, err := p.Lookup("Name")
	if err != nil {
		fatalf("Lookup(%q) on %s: %v", "Name", what, err)
	}
	return f.(func() string)()
}

// nextOffset finds where moduledata.next lives without this fixture
// knowing the runtime's layout: node is a chained moduledata whose
// successor is succ, and succ is the current chain tail, so one
// pointer-sized word in node holds succ's address and the same word in
// succ is nil.
//
// The scan takes the FIRST word of node holding succ and stops there.
// Stopping matters: a moduledata is a few hundred bytes, so a scan
// that runs to a fixed limit reads well past the end of the host's
// node into unrelated globals — and runtime.lastmoduledatap holds
// exactly succ once part 1's rejection has retreated it, so a wide
// window can read that as a second "next link" and fail a completely
// correct runtime with a message blaming the runtime for the
// fixture's over-read.
//
// Confirming the hit against succ replaces the uniqueness count: a
// coincidental match on some earlier field would have to read zero in
// succ too, and this is the word part 2 writes into succ, so the check
// also proves that write lands on a free link rather than a live
// field.
//
// node must be the HOST's moduledata, which is what the only caller
// passes. On the no-hit path — a broken fixture premise — the scan runs
// the full limit and so reads past the end of node into whatever the
// linker placed after it; inside the host image's data that is mapped
// memory, and the fatalf below still reports the premise failure. A
// node at the end of a DLL's data section would have no such guarantee.
func nextOffset(node, succ uintptr) uintptr {
	const limit = 2048 // a moduledata is a few hundred bytes; no hit at all is a fixture bug
	for off := uintptr(0); off+8 <= limit; off += 8 {
		if *(*uintptr)(unsafe.Pointer(node + off)) != succ {
			continue
		}
		if tail := *(*uintptr)(unsafe.Pointer(succ + off)); tail != 0 {
			fatalf("the word at offset %d holds the successor's address but reads %#x in the successor, which is the chain tail; want 0 (the next link)", off, tail)
		}
		return off
	}
	fatalf("no word in the first %d bytes of the tail moduledata holds its successor's address; want the next link", limit)
	return 0
}

func main() {
	want, err := hex.DecodeString(os.Args[len(os.Args)-1])
	if err != nil || len(want) != 8 {
		fatalf("bad {Marker, Version} hex %q: %v", os.Args[len(os.Args)-1], err)
	}
	version := binary.LittleEndian.Uint32(want[4:])
	bad := append([]byte(nil), want...)
	binary.LittleEndian.PutUint32(bad[4:], version+1)

	cwd, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, "reject-a", "p.dll"))
	if err != nil {
		fatalf("read victim: %v", err)
	}
	// Exactly one occurrence is the premise of the byte patch: Marker
	// is a distinctive magic ("ABIP"), so a second hit would mean the
	// pair appears somewhere other than the go:plugin.abi export, and
	// rewriting every hit would corrupt unrelated image bytes into an
	// undiagnosable load failure instead of testing the marker.
	if n := bytes.Count(raw, want); n != 1 {
		fatalf("found %d occurrences of the {Marker, Version} pair in the victim DLL; want exactly 1", n)
	}
	patched := bytes.Replace(raw, want, bad, 1)

	// The peer goes on the chain first, so prev below is a plugin's
	// moduledata and not &firstmoduledata. hostTail/prev also give
	// nextOffset a node whose successor is known.
	hostTail := chainTail()
	peer, err := plugin.Open(filepath.Join(cwd, "reject-peer.dll"))
	if err != nil {
		fatalf("peer plugin failed to load: %v", err)
	}
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q; want P", got)
	}
	prev := chainTail()
	if prev == hostTail {
		fatalf("peer plugin did not chain its moduledata; fixture premise broken")
	}

	// Part 1: the stage-2 rejection through plugin.Open. The file at
	// swap holds the healthy bytes when stage 1 reads it and the
	// patched bytes when LoadLibrary maps it.
	swap := filepath.Join(cwd, "reject-swap.dll")
	if err := os.WriteFile(swap, raw, 0o777); err != nil {
		fatalf("write swap copy: %v", err)
	}
	hookCalls := 0
	testHookBetweenFenceStages = func(path string) {
		hookCalls++
		if !strings.EqualFold(path, swap) {
			fatalf("fence hook saw path %q; want %q", path, swap)
		}
		// Stage 1 has closed its read handle and nothing has mapped
		// the file yet, so the rewrite is an ordinary overwrite.
		if err := os.WriteFile(swap, patched, 0o777); err != nil {
			fatalf("swap in the patched bytes: %v", err)
		}
	}
	_, err = plugin.Open(swap)
	if err == nil {
		fatalf("Open of the swapped plugin succeeded; want the stage-2 rejection")
	}
	wantMsg := fmt.Sprintf("the loaded plugin module was built with an incompatible toolchain ABI (plugin ABI %d, host ABI %d)", version+1, version)
	if !strings.Contains(err.Error(), wantMsg) {
		fatalf("Open(swap) = %v; want the stage-2 text %q", err, wantMsg)
	}
	if strings.Contains(err.Error(), "(previous failure)") {
		fatalf("first Open(swap) reported a cached failure: %v", err)
	}
	if hookCalls != 1 {
		fatalf("fence hook ran %d times during the first Open; want 1", hookCalls)
	}
	if now := chainTail(); now != prev {
		fatalf("after the stage-2 rejection the chain tail moved from %#x to %#x: the rejected node is still chained", prev, now)
	}
	// The rejection must keep the mapping resident (its DllMain chained
	// into this process; unmapping needs the barrier only Close takes)
	// and the module it judged must be the swapped-in one.
	swapU16, err := syscall.UTF16PtrFromString(swap)
	if err != nil {
		fatalf("%v", err)
	}
	// KeepAlive, because LazyProc.Call is not one of the functions the
	// unsafe.Pointer rules exempt (only a direct syscall.Syscall/SyscallN
	// call is): it boxes its arguments into a []uintptr and allocates on
	// the way there, so without this the only reference to swapU16 ends
	// at the conversion above and a GC in that window can free the UTF-16
	// buffer out from under GetModuleHandleW.
	h, _, _ := procGetModuleHandle.Call(uintptr(unsafe.Pointer(swapU16)))
	runtime.KeepAlive(swapU16)
	if h == 0 {
		fatalf("the rejected module is no longer mapped; the stage-2 rejection must not FreeLibrary a chained image")
	}
	proc, procErr := syscall.GetProcAddress(syscall.Handle(h), "go:plugin.abi")
	if procErr != nil {
		fatalf("rejected module exports no go:plugin.abi: %v", procErr)
	}
	if got := *(*[8]byte)(unsafe.Pointer(proc)); !bytes.Equal(got[:], bad) {
		fatalf("mapped module carries pair % x; want the swapped-in % x", got[:], bad)
	}
	// A later Open answers from the negative cache: same text, marked
	// as a previous failure, and the file is not read again — the
	// hook, which sits after stage 1's read, must not run.
	_, err = plugin.Open(swap)
	if err == nil {
		fatalf("second Open of the rejected plugin succeeded; want the cached failure")
	}
	if !strings.Contains(err.Error(), wantMsg) || !strings.Contains(err.Error(), "(previous failure)") {
		fatalf("second Open(swap) = %v; want %q marked \"(previous failure)\"", err, wantMsg)
	}
	if hookCalls != 1 {
		fatalf("fence hook ran %d times in total; the cached failure must short-circuit before stage 1", hookCalls)
	}
	testHookBetweenFenceStages = nil

	// Part 2: a node the host may not read. prev is still the tail, so
	// its next link is free; point it at a page no access mode can
	// read and run the rejection treatment exactly as open does —
	// growth check on prev's own link, then the positional truncation.
	// Any dereference of the fake node faults the process.
	off := nextOffset(hostTail, prev)
	fake, _, allocErr := procVirtualAlloc.Call(0, 4096, 0x3000 /* MEM_COMMIT|MEM_RESERVE */, 0x01 /* PAGE_NOACCESS */)
	if fake == 0 {
		fatalf("VirtualAlloc(PAGE_NOACCESS): %v", allocErr)
	}
	// No collector may walk the chain while the fake node is linked;
	// the window is a handful of instructions with no allocation, but
	// a GC cycle scheduled into it would be a false failure.
	gcPercent := debug.SetGCPercent(-1)
	*(*uintptr)(unsafe.Pointer(prev + off)) = fake
	if !chainGrew(prev) {
		fatalf("chainGrew reports no growth after linking a node at offset %d of the tail moduledata; either chainGrew is broken or nextOffset returned a word that is not the next link", off)
	}
	rejectChainedModule(prev)
	debug.SetGCPercent(gcPercent)
	if now := chainTail(); now != prev {
		fatalf("after rejecting the unreadable node the chain tail is %#x; want %#x", now, prev)
	}
	if chainGrew(prev) {
		fatalf("chainGrew still reports growth after the rejection")
	}

	// The peer was chained before both rejections and must come through
	// them untouched: still on the chain, still in activeModules, still
	// resolving its own symbols.
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q after the rejections; want P", got)
	}

	// A healthy sibling must load cleanly afterwards with the rejected,
	// still-resident image sitting in the address space — which it can
	// only do if the truncation moved addmoduledata's append cursor
	// back with the chain tail. Both copies share pkghashes (identical
	// sources), so this load also proves the peer scan does not trip
	// over the residue.
	pb, err := plugin.Open(filepath.Join(cwd, "reject-b", "p.dll"))
	if err != nil {
		fatalf("healthy sibling failed to load after the rejections: %v", err)
	}
	if got := name(pb, "sibling"); got != "V" {
		fatalf("sibling Name()=%q; want V", got)
	}
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q after the later load; want P", got)
	}
	fmt.Println("PASS")
}
