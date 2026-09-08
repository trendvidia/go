// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"plugin"
)

func main() {
	p, err := plugin.Open("cgonotify-bare.dll")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Open:", err)
		os.Exit(1)
	}
	f, err := p.Lookup("F")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Lookup F:", err)
		os.Exit(1)
	}
	if got := f.(func() int)(); got != 7 {
		fmt.Fprintf(os.Stderr, "F() = %d, want 7\n", got)
		os.Exit(1)
	}
	fmt.Println("ok")
}
