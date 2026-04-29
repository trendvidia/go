// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Second plugin: also touches common to confirm both plugins share
// the host's common.X (single moduledata for the package).
package main

import "testplugin/common"

func init() {
	common.X = 2
}

func G() int { return 11 }

func main() {}
