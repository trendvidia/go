// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testplugin/inlclosure/lib"

func Get() string { return lib.Apply(lib.Desc("A")) }
