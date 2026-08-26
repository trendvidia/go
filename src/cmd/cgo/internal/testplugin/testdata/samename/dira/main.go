// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// dira is one half of the same-base-name pair: a plugin whose only
// observable is which of the two same-named DLLs answered. Kept
// dependency-free so the pair shares nothing but the runtime and std,
// letting any divergence between them be attributed to identity, not
// to package version locks.
package main

// Name identifies which same-named plugin answered.
func Name() string { return "A" }
