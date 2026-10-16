// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

package testfastleaf

/*
#cgo fastleaf sleepLeaf
#cgo noescape sleepPlain
#include <errno.h>
#include <time.h>

// sleepLeaf and sleepPlain sleep ns nanoseconds once and report whether
// the sleep was interrupted by a signal (EINTR).
static int sleepLeaf(long ns) {
	struct timespec ts = {0, ns};
	return nanosleep(&ts, NULL) != 0 && errno == EINTR;
}
static int sleepPlain(long ns) {
	struct timespec ts = {0, ns};
	return nanosleep(&ts, NULL) != 0 && errno == EINTR;
}
*/
import "C"

// SleepLeaf sleeps in a fastleaf C call and reports whether a signal
// interrupted it.
func SleepLeaf(ns int) bool { return C.sleepLeaf(C.long(ns)) != 0 }

// SleepPlain is SleepLeaf through an ordinary cgo call.
func SleepPlain(ns int) bool { return C.sleepPlain(C.long(ns)) != 0 }
