// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package common is shared between host and plugins so we can
// observe plugin loads from the host (X is mutated by plugin
// inits).
package common

var X int = 3
