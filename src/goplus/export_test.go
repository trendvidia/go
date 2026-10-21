// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "unsafe"

// AllocDomainCounter returns the runtime object that holds d's count.
func AllocDomainCounter(d *AllocDomain) unsafe.Pointer { return d.d }

// LayoutType returns the runtime type behind l.
func LayoutType(l *Layout) unsafe.Pointer { return l.typ }

// SampleTagState returns the runtime object that holds t's estimate.
func SampleTagState(t *SampleTag) unsafe.Pointer { return t.t }
