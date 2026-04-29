// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package common is the alt-version: identical API but different
// initial value so the package's pkghash differs from the canonical
// common's. A plugin built against this package against an alt
// GOPATH must fail plugin.Open with a "different version" error.
package common

var X int = 4
