// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

var GoplusSampleWeight = goplusSampleWeight

// Fastexprand draws the gap to the heap profiler's next sample.
func Fastexprand(mean int) int32 { return fastexprand(mean) }
