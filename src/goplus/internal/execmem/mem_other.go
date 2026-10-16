// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !darwin && !linux && !windows

package execmem

import "errors"

func mapExec(code []byte) ([]byte, error) {
	return nil, errors.New("execmem: unsupported platform")
}

func unmapExec(mem []byte) error { return nil }
