// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !arm64

package goplus_test

import "goplus"

func storeCode(flag, entry uintptr, barrier bool) []byte { return nil }
func probeCode(entry uintptr, calls int) []byte          { return nil }

const (
	loopPlain = iota
	loopInline
	loopHelper
)

func storeLoopCode(mode int, flag, entry, helper uintptr) []byte { return nil }

func frameCode() ([]byte, []goplus.FrameStep, uint32) { return nil, nil, 0 }

var framePointerSlots []uint32

var (
	abiIntRegs   []byte
	abiFloatRegs int
)

func abiProbeCode(out uintptr) []byte { return nil }

type abiProbeFunc = func()

func callABIProbe(f abiProbeFunc) []uintptr { return nil }
