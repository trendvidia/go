// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

// Command rejectchained's stub for platforms whose loader half of the
// fixture does not exist; see host_windows.go.
package main

import "fmt"

func main() {
	fmt.Println("skip: rejectchained is windows-only")
}
