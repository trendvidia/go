// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command sleepleaf sleeps in a #cgo fastleaf C function for the number
// of milliseconds given as its argument, then prints "slept". It is
// built with -race by TestFastLeafCheckRace.
package main

/*
#cgo fastleaf sleepLeaf

#ifdef _WIN32
#include <windows.h>
static void sleepLeaf(int ms) { Sleep(ms); }
#else
#include <time.h>
static void sleepLeaf(int ms) {
	struct timespec ts = { ms / 1000, (long)(ms % 1000) * 1000000L };
	while (nanosleep(&ts, &ts) != 0) {
	}
}
#endif
*/
import "C"

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	ms, err := strconv.Atoi(os.Args[1])
	if err != nil {
		panic(err)
	}
	C.sleepLeaf(C.int(ms))
	fmt.Println("slept")
}
