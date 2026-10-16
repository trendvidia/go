// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Allocation domains (goplus.AllocDomain): count the heap bytes allocated
// by a group of goroutines.

package runtime

import (
	"internal/cpu"
	"internal/runtime/atomic"
	"unsafe"
)

// goplusAllocDomain counts bytes allocated by goroutines whose
// g.allocDomain points to it. The count is striped by P so allocating
// goroutines on different Ps rarely share a cache line; a stripe can be
// shared when GOMAXPROCS exceeds the stripe count, hence the atomic add.
type goplusAllocDomain struct {
	stripes [64]struct {
		n atomic.Uint64
		_ [cpu.CacheLinePadSize - 8]byte
	}
}

// add counts n bytes allocated by the current goroutine. Allocation calls
// it only when the goroutine is in a domain, so it is kept out of line.
//
//go:noinline
func (d *goplusAllocDomain) add(n uintptr) {
	i := 0
	if pp := getg().m.p.ptr(); pp != nil {
		i = int(pp.id) & (len(d.stripes) - 1)
	}
	d.stripes[i].n.Add(int64(n))
}

//go:linkname goplus_newAllocDomain goplus.runtime_newAllocDomain
func goplus_newAllocDomain() unsafe.Pointer {
	return unsafe.Pointer(new(goplusAllocDomain))
}

// goplus_setAllocDomain puts the calling goroutine in domain d (nil for
// none) and returns its previous domain. newproc1 copies the domain to
// the goroutines it starts.
//
//go:linkname goplus_setAllocDomain goplus.runtime_setAllocDomain
func goplus_setAllocDomain(d unsafe.Pointer) (prev unsafe.Pointer) {
	gp := getg()
	prev = unsafe.Pointer(gp.allocDomain)
	gp.allocDomain = (*goplusAllocDomain)(d)
	return prev
}

//go:linkname goplus_allocDomainTotal goplus.runtime_allocDomainTotal
func goplus_allocDomainTotal(d unsafe.Pointer) uint64 {
	dom := (*goplusAllocDomain)(d)
	var total uint64
	for i := range dom.stripes {
		total += dom.stripes[i].n.Load()
	}
	return total
}
