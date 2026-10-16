// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

package testfastleaf_test

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

// waitForStdin prints checkReady, then parks until stdin is readable,
// through the network poller, and returns without a system call: one
// would wake sysmon, which is what TestFastLeafCheckIdle must not have.
func waitForStdin(t *testing.T) {
	if err := syscall.SetNonblock(0, true); err != nil {
		t.Fatal(err)
	}
	rc, err := os.NewFile(0, "stdin").SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(checkReady)
	ready := time.Now()
	parked := false
	err = rc.Read(func(uintptr) bool {
		if !parked {
			parked = true
			return false
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	// The parent writes 300ms after checkReady. Returning much sooner
	// means stdin was never waited on, and sysmon may never have slept.
	if waited := time.Since(ready); waited < 200*time.Millisecond {
		t.Fatalf("stdin read returned after %v, before the parent wrote", waited)
	}
}
