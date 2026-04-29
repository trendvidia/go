// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Mismatched plugin: identical to ../../p1/p1.go but built from an
// alt GOPATH where common's content differs, so the plugin links
// against a different version of the common package and the runtime
// must reject it on Open via the pkghashes guard (Bug A).
package main

import "testplugin/common"

func F() int { return 3 }

func ReadCommonX() int { return common.X }

func main() {}
