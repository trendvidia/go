// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !darwin && !linux && !openbsd

package runtime

// osThreadStartPC is 0 where newosproc does not use threadStart.
func osThreadStartPC() uintptr { return 0 }
