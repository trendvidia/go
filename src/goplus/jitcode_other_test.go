// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !arm64

package goplus_test

func storeCode(flag, entry uintptr, barrier bool) []byte { return nil }
func probeCode(entry uintptr, calls int) []byte          { return nil }

const (
	loopPlain = iota
	loopInline
	loopHelper
)

func storeLoopCode(mode int, flag, entry, helper uintptr) []byte { return nil }
