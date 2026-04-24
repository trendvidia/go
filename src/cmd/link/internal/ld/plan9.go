// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Plan9-specific linker helpers.
//
// This file is the destination for Plan9-specific logic that previously
// lived inline in shared orchestration files. Pre-existing Plan9 code in
// asmb.go/symtab.go/main.go/lib.go is intentionally left in place for
// Phase 1 — only new extractions land here.

package ld

import "cmd/link/internal/sym"

// plan9SegmentFileOffset returns the file offset for a segment placed
// after prev in a Plan9 a.out binary. Plan9 segments are tightly packed
// with no alignment beyond what the previous segment's file length
// provides.
func plan9SegmentFileOffset(prev *sym.Segment) uint64 {
	return prev.Fileoff + prev.Filelen
}
