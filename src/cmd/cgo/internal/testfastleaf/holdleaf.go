// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package testfastleaf

/*
#cgo fastleaf holdLeaf

#ifdef _WIN32
#include <windows.h>
static void holdLeaf(int ms) { Sleep(ms); }
#else
#include <time.h>
static void holdLeaf(int ms) {
	struct timespec ts = { ms / 1000, (long)(ms % 1000) * 1000000L };
	while (nanosleep(&ts, &ts) != 0) {
	}
}
#endif
*/
import "C"

// HoldLeaf sleeps for ms milliseconds in a fastleaf C function: a call
// that runs far longer than a fastleaf function may.
func HoldLeaf(ms int) { C.holdLeaf(C.int(ms)) }
