// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package wrap is linked ONLY by the plugins, but its statically
// initialized func-valued var points at a function whose body is the
// HOST's. That is trendvidia/go#106's literal shape: the compiler emits
// the word as data with an R_ADDR to the PLUGIN's `shared.Fn·f`, which
// never passes through a go.funcptr slot, so nothing canonicalised it
// until the host began publishing its funcvals.
package wrap

import "testplugin/funcident/shared"

// FuncVar is the statically initialized word under test.
var FuncVar func() int = shared.Fn
