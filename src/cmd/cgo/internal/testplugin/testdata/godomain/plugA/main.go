// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"go.example.org/fakemod/hostlib"
	"go.example.org/fakemod/lib"
)

// BumpA bumps lib.Counter by 1 and returns it.
func BumpA() int {
	return lib.Bump(1)
}

// SetShared writes the host-shared hostlib.Value.
func SetShared(n int) {
	hostlib.Value = n
}

// GetShared reads the host-shared hostlib.Value.
func GetShared() int {
	return hostlib.Value
}

func main() {}
