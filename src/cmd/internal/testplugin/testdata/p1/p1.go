// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Pure-Go plugin: no `import "C"`. Demonstrates basic plugin.Lookup
// targets (function, exported variable) and shared-package mutation
// across the host/plugin boundary.
package main

import "testplugin/common"

var Seven int

func init() {
	Seven = 7
	common.X *= Seven
}

func F() int { return 3 }

func ReadCommonX() int { return common.X }

func main() {}
