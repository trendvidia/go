// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package execmem

import (
	"syscall"
	"unsafe"
)

var (
	modkernel32               = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc          = modkernel32.NewProc("VirtualAlloc")
	procVirtualProtect        = modkernel32.NewProc("VirtualProtect")
	procVirtualFree           = modkernel32.NewProc("VirtualFree")
	procFlushInstructionCache = modkernel32.NewProc("FlushInstructionCache")
	procGetCurrentProcess     = modkernel32.NewProc("GetCurrentProcess")
)

const (
	memCommit       = 0x1000
	memReserve      = 0x2000
	memRelease      = 0x8000
	pageReadWrite   = 0x04
	pageExecuteRead = 0x20
)

func mapExec(code []byte) ([]byte, error) {
	size := uintptr(len(code)+0xfff) &^ 0xfff
	addr, _, err := procVirtualAlloc.Call(0, size, memCommit|memReserve, pageReadWrite)
	if addr == 0 {
		return nil, err
	}
	mem := unsafe.Slice((*byte)(unsafe.Pointer(addr)), size)
	copy(mem, code)
	var old uint32
	if r, _, err := procVirtualProtect.Call(addr, size, pageExecuteRead, uintptr(unsafe.Pointer(&old))); r == 0 {
		procVirtualFree.Call(addr, 0, memRelease)
		return nil, err
	}
	proc, _, _ := procGetCurrentProcess.Call()
	if r, _, err := procFlushInstructionCache.Call(proc, addr, uintptr(len(code))); r == 0 {
		procVirtualFree.Call(addr, 0, memRelease)
		return nil, err
	}
	return mem, nil
}

func unmapExec(mem []byte) error {
	if r, _, err := procVirtualFree.Call(uintptr(unsafe.Pointer(&mem[0])), 0, memRelease); r == 0 {
		return err
	}
	return nil
}
