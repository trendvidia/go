// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Version 2 of the same extension — an upgrade. It adds state AHEAD of
// the fields v1 exported, so their offsets move. That is the whole
// point of the fixture: a stale pointer into v1 does not land on v2's
// copy of the same variable, it lands on whatever v2 put there.
package main

var newTable [96]int

// Budget now precedes Threshold, and sits at v1's Threshold offset.
var Budget int = 2000

// Threshold has moved.
var Threshold int = 7

func Report() (int, int) { return Threshold, Budget }

func touch() int { return newTable[0] }

func main() { _ = touch() }
