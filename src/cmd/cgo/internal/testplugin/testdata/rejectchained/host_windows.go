// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

// Command rejectchained pins the stage-2 ABI rejection's residue
// handling: a plugin DLL whose go:plugin.abi marker names another
// toolchain generation, mapped raw so its DllMain chains its
// moduledata into this process, must leave no walkable chain node
// behind once rejected — while a plugin chained BEFORE it survives
// untouched and a healthy sibling still loads cleanly afterwards.
//
// The peer is opened first on purpose. The rejection truncates the
// chain at the caller's pre-load tail, so a run with an empty plugin
// chain cannot tell that apart from truncating every plugin off the
// host; only a tail that is itself a plugin's moduledata does.
//
// The fixture needs no foreign toolchain: it loads a healthy plugin
// built by this toolchain after flipping its marker's Version word in
// the file bytes, so the loader maps and initializes it exactly as it
// would a real swapped-in incompatible build — only the marker word
// differs. The harness passes the correct {Marker, Version} pair as a
// hex argument so nothing here is hardcoded against future bumps.
//
// The patched module is deliberately never freed: it chained real Go
// module data into this host, and dropping its loader reference would
// unmap a chained image — exactly what the fence refuses to do. One
// mapping stays resident for the life of the process.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"syscall"
	"unsafe"
)

//go:linkname chainTail plugin.chainTail
func chainTail() uintptr

//go:linkname rejectChainedModule plugin.rejectChainedModule
func rejectChainedModule(prevTail uintptr)

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

func main() {
	want, err := hex.DecodeString(os.Args[len(os.Args)-1])
	if err != nil || len(want) != 8 {
		fatalf("bad {Marker, Version} hex %q: %v", os.Args[len(os.Args)-1], err)
	}
	bad := append([]byte(nil), want...)
	binary.LittleEndian.PutUint32(bad[4:], binary.LittleEndian.Uint32(want[4:])+1)

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
	patched := filepath.Join(cwd, "reject-patched.dll")
	if err := os.WriteFile(patched, bytes.Replace(raw, want, bad, 1), 0o777); err != nil {
		fatalf("write patched copy: %v", err)
	}

	// The peer goes on the chain first, so prev below is a plugin's
	// moduledata and not &firstmoduledata.
	peer, err := plugin.Open(filepath.Join(cwd, "reject-peer.dll"))
	if err != nil {
		fatalf("peer plugin failed to load: %v", err)
	}
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q; want P", got)
	}

	prev := chainTail()
	h, err := syscall.LoadLibrary(patched)
	if err != nil {
		fatalf("LoadLibrary(patched) failed: %v", err)
	}
	proc, procErr := syscall.GetProcAddress(h, "go:plugin.abi")
	if procErr != nil {
		fatalf("patched module exports no go:plugin.abi: %v", procErr)
	}
	got := *(*[8]byte)(unsafe.Pointer(proc))
	if !bytes.Equal(got[:], bad) {
		fatalf("mapped module carries pair % x; want the patched % x", got[:], bad)
	}
	if chainTail() == prev {
		fatalf("patched module did not chain its moduledata; fixture premise broken")
	}

	// The treatment under test: what open()'s stage-2 rejection runs.
	// prev is the pre-load tail, exactly as open passes its own
	// snapshot; the residue is identified by position, never by reading
	// a field of the rejected module.
	rejectChainedModule(prev)
	if now := chainTail(); now != prev {
		fatalf("after rejection the chain tail moved from %#x to %#x: the rejected node is still chained", prev, now)
	}

	// The peer was chained before the rejected module and must come
	// through it untouched: still on the chain, still in activeModules,
	// still resolving its own symbols.
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q after the rejection; want P", got)
	}

	// A healthy sibling must load cleanly afterwards with the rejected,
	// still-resident image sitting in the address space — which it can
	// only do if the truncation moved addmoduledata's append cursor
	// back with the chain tail. Both copies share pkghashes (identical
	// sources), so this load also proves the peer scan does not trip
	// over the residue.
	pb, err := plugin.Open(filepath.Join(cwd, "reject-b", "p.dll"))
	if err != nil {
		fatalf("healthy sibling failed to load after the rejection: %v", err)
	}
	if got := name(pb, "sibling"); got != "V" {
		fatalf("sibling Name()=%q; want V", got)
	}
	if got := name(peer, "peer"); got != "P" {
		fatalf("peer Name()=%q after the later load; want P", got)
	}
	fmt.Println("PASS")
}
