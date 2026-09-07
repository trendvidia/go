// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	rootcom "example.com"
	rootorg "example.org"
)

// BumpA bumps rootcom.Counter by 1 and returns it.
func BumpA() int {
	return rootcom.Bump(1)
}

// SetShared writes the host-shared rootorg.Value.
func SetShared(n int) {
	rootorg.Value = n
}

// GetShared reads the host-shared rootorg.Value.
func GetShared() int {
	return rootorg.Value
}

func main() {}
