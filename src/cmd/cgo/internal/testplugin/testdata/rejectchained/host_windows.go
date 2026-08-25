// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

// Command rejectchained pins the stage-2 ABI rejection's residue
// handling: a plugin DLL whose go:plugin.abi marker names another
// toolchain generation, mapped raw so its DllMain chains its
// moduledata into this process, must leave no walkable chain node
// behind once rejected — and a healthy sibling must still load
// cleanly afterwards.
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
func rejectChainedModule()

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rejectchained: "+format+"\n", args...)
	os.Exit(1)
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
	if bytes.Count(raw, want) == 0 {
		fatalf("{Marker, Version} pair not found in the victim DLL")
	}
	patched := filepath.Join(cwd, "reject-patched.dll")
	if err := os.WriteFile(patched, bytes.ReplaceAll(raw, want, bad), 0o777); err != nil {
		fatalf("write patched copy: %v", err)
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
	rejectChainedModule()
	if now := chainTail(); now != prev {
		fatalf("after rejection the chain tail moved from %#x to %#x: the rejected node is still chained", prev, now)
	}

	// A healthy sibling must load cleanly afterwards with the rejected,
	// still-resident image sitting in the address space. Both copies
	// share pkghashes (identical sources), so this load also proves the
	// peer scan does not trip over the residue.
	pb, err := plugin.Open(filepath.Join(cwd, "reject-b", "p.dll"))
	if err != nil {
		fatalf("healthy sibling failed to load after the rejection: %v", err)
	}
	f, err := pb.Lookup("Name")
	if err != nil {
		fatalf("Lookup on healthy sibling: %v", err)
	}
	if got := f.(func() string)(); got != "V" {
		fatalf("sibling Name()=%q; want V", got)
	}
	fmt.Println("PASS")
}
