// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// crypto/ed25519 is the point: the host does not link it, so the
// package's assembly — crypto/internal/fips140/edwards25519/field's
// feSquare and friends — is PLUGIN-local, and so is the g-reload the
// compiler emits after each ABI0 call to it. edwards25519's own init
// calls into that path, so a broken reload faults during plugin.Open,
// before any Lookup could guard against it (trendvidia/go#117).
import "crypto/ed25519"

// KeyLen exercises the same assembly again after load.
func KeyLen() int {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		return -1
	}
	return len(pub)
}

func main() {}
