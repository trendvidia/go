// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || linux

package execmem

import "syscall"

func mapExec(code []byte) ([]byte, error) {
	size := (len(code) + syscall.Getpagesize() - 1) &^ (syscall.Getpagesize() - 1)
	mem, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	copy(mem, code)
	if err := syscall.Mprotect(mem, syscall.PROT_READ|syscall.PROT_EXEC); err != nil {
		syscall.Munmap(mem)
		return nil, err
	}
	flushICache(mem[:len(code)])
	return mem, nil
}

func unmapExec(mem []byte) error {
	return syscall.Munmap(mem)
}
