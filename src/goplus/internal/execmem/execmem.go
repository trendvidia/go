// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package execmem maps machine code generated at run time so that it can
// be called, for testing package goplus's hooks for generated code.
//
// It supports amd64 and arm64 on darwin, linux and windows. Code is
// entered through a Go func value, so it is called with Go's internal
// register ABI (ABIInternal): arguments arrive in AX, BX, CX, ... on
// amd64 and R0, R1, R2, ... on arm64, and the g register (R14 on amd64,
// R28 on arm64) holds the calling goroutine.
//
// Code running from this memory is not known to the runtime. It must not
// let the goroutine stop at a point where the runtime would walk its
// stack; see goplus.
package execmem

import (
	"errors"
	"runtime"
	"unsafe"
)

// Supported reports whether this platform can map and run generated code.
func Supported() bool {
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return false
	}
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return true
	}
	return false
}

// Code is machine code mapped executable.
type Code struct {
	mem  []byte // the whole mapping
	addr uintptr
}

// Map copies code into newly mapped memory, makes it executable and
// read-only, and flushes the instruction cache for it.
func Map(code []byte) (*Code, error) {
	if !Supported() {
		return nil, errors.New("execmem: unsupported platform")
	}
	if len(code) == 0 {
		return nil, errors.New("execmem: no code")
	}
	mem, err := mapExec(code)
	if err != nil {
		return nil, err
	}
	return &Code{mem: mem, addr: uintptr(unsafe.Pointer(&mem[0]))}, nil
}

// Addr returns the address of the first instruction.
func (c *Code) Addr() uintptr { return c.addr }

// Unmap releases the memory. The code must not be running, and must not
// be called again.
func (c *Code) Unmap() error {
	err := unmapExec(c.mem)
	c.mem = nil
	return err
}

// funcval is the layout of a Go func value: a pointer to a word holding
// the entry PC.
type funcval struct{ fn uintptr }

// Func returns c as a Go function of type F, which must be a func type
// whose signature matches what the code expects under ABIInternal. The
// result is only valid while c is mapped.
func Func[F any](c *Code) F {
	fv := &funcval{c.addr}
	return *(*F)(unsafe.Pointer(&fv))
}
