// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package shared is linked by the funcident host AND both plugins, so
// Fn's body lives in the host and each plugin reaches it through the
// import library. Each module still emits its OWN `shared.Fn·f` — the
// one-word funcval object a func VALUE points at — and a plugin's copy
// holds the address of that plugin's IAT thunk, not the host's code.
// Canonicalising those copies is what trendvidia/go-archive#106 is about.
package shared

// Fn is the function whose value identity is under test.
func Fn() int { return 7 }

// FuncVar is a statically initialized func-valued var in a package the
// host links, so the VARIABLE is host-shared and no module reads a
// local copy of the word. It is here to pin that this case keeps
// working, not because it was ever broken.
var FuncVar func() int = Fn
