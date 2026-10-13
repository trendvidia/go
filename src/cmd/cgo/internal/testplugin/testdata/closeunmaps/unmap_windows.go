// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// MEMORY_BASIC_INFORMATION mirrors the Win32 struct of the same
// name. VirtualQuery fills it in for the region containing the
// queried address. We only consult State and BaseAddress here.
type memoryBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	PartitionId       uint16
	_                 uint16 // padding
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

const (
	memCommit  = 0x1000
	memReserve = 0x2000
	memFree    = 0x10000
	memImage   = 0x1000000
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procVirtualQuery = kernel32.NewProc("VirtualQuery")
)

// isUnmapped probes the region containing addr with VirtualQuery.
// FreeLibrary releases the plugin's image mapping, and the question
// this fixture asks is whether the IMAGE is gone — not whether the
// address range went back to the free list.
//
// Those stopped being the same thing with trendvidia/go-archive#122:
// unloadPlugin now reserves the range the image occupied, so the
// loader cannot reuse it and a stale pointer faults instead of
// silently addressing the next plugin. A reserved region reports
// MEM_RESERVE with Type 0 — no committed pages, no image, nothing
// mapped; accessing it faults exactly as MEM_FREE would. Treating
// that as "still mapped" would fail this fixture for the mechanism
// that makes unload safe.
//
// What "still loaded" actually looks like is MEM_COMMIT with
// Type == MEM_IMAGE, and that is what this must still catch.
//
// Returns: 1 = image unmapped (MEM_FREE, or MEM_RESERVE over
// non-image memory), 0 = still mapped, -1 = VirtualQuery failed
// (errnoOut holds the Windows error code from GetLastError).
func isUnmapped(addr uintptr) (status int, errnoOut int) {
	var mbi memoryBasicInformation
	r1, _, e1 := procVirtualQuery.Call(addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
	if r1 == 0 {
		if errno, ok := e1.(syscall.Errno); ok {
			return -1, int(errno)
		}
		return -1, 0
	}
	switch {
	case mbi.State == memFree:
		return 1, 0
	case mbi.State == memReserve && mbi.Type != memImage:
		// The trendvidia/go-archive#122 reservation: address space held, nothing mapped.
		return 1, 0
	}
	return 0, 0
}
