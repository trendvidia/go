package main

import (
	"syscall"
	"unsafe"
)

type memSample struct {
	reserved uint64 // address space reserved but not committed
	private  uint64 // committed private bytes
}

type memoryBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	PartitionId       uint16
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

const (
	memCommit  = 0x1000
	memReserve = 0x2000
	memImage   = 0x1000000
)

// sampleMem walks the process address space. The reservation Close
// leaves behind on unload (trendvidia/go-archive#122/#123) shows up as
// MEM_RESERVE that is not MEM_IMAGE, which is exactly what accumulates
// per upgrade cycle and what a long-running trading process has to
// budget for.
func sampleMem() memSample {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	virtualQuery := k32.NewProc("VirtualQuery")
	var s memSample
	var addr uintptr
	var mbi memoryBasicInformation
	for i := 0; i < 1<<22; i++ {
		r, _, _ := virtualQuery.Call(addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if r == 0 {
			break
		}
		switch {
		case mbi.State == memReserve && mbi.Type != memImage:
			s.reserved += uint64(mbi.RegionSize)
		case mbi.State == memCommit && mbi.Type == 0x20000: // MEM_PRIVATE
			s.private += uint64(mbi.RegionSize)
		}
		next := mbi.BaseAddress + mbi.RegionSize
		if next <= addr {
			break
		}
		addr = next
	}
	return s
}
