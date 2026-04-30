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
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procVirtualQuery = kernel32.NewProc("VirtualQuery")
)

// isUnmapped probes the region containing addr with VirtualQuery.
// FreeLibrary releases the plugin's image mapping; the formerly-
// mapped region then reports State == MEM_FREE (0x10000).
//
// Returns: 1 = unmapped (MEM_FREE), 0 = mapped (MEM_COMMIT or
// MEM_RESERVE), -1 = VirtualQuery failed (errnoOut holds the
// Windows error code from GetLastError).
func isUnmapped(addr uintptr) (status int, errnoOut int) {
	var mbi memoryBasicInformation
	r1, _, e1 := procVirtualQuery.Call(addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
	if r1 == 0 {
		if errno, ok := e1.(syscall.Errno); ok {
			return -1, int(errno)
		}
		return -1, 0
	}
	if mbi.State == memFree {
		return 1, 0
	}
	return 0, 0
}
