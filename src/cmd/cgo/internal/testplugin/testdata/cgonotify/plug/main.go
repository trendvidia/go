// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A cgo plugin that calls back from C into Go — the direction whose
// entry point cgo guards with _cgo_wait_runtime_init_done, and so the
// direction that needs this image's own libcgo to have been notified
// that the runtime is up. See trendvidia/go#62.
package main

/*
#include <stdlib.h>
extern int GoAdd(int);
static int callback_via_c(int x) { return GoAdd(x); }
*/
import "C"

//export GoAdd
func GoAdd(x C.int) C.int { return x + 1 }

// GoToC is the unguarded direction: cgo emits no handshake here, so it
// completes whether or not the notify export survived. It is in the
// fixture to keep CToGo's result meaningful — if both hang, the fault is
// not the handshake.
func GoToC() int { return int(C.abs(C.int(-7))) }

// CToGo goes Go -> C -> Go. Its C wrapper opens with
// _cgo_wait_runtime_init_done, which blocks until plugin.Open has called
// x_cgo_notify_runtime_init_done through this DLL's export table.
func CToGo() int { return int(C.callback_via_c(C.int(41))) }

func main() {}
