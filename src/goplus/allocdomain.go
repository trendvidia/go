// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "unsafe"

// An AllocDomain counts the heap bytes allocated by a group of
// goroutines: those that run code inside [AllocDomain.Run], and every
// goroutine they start, directly or indirectly, including goroutines
// that outlive the call to Run.
//
// The count is of bytes requested: an allocation of n bytes adds n,
// before the allocator rounds it up to a size class, so it is smaller
// than the matching growth of runtime.MemStats.TotalAlloc by that
// rounding. Allocations the runtime makes on another goroutine on the
// domain's behalf are not counted: a finalizer, a cleanup, or a function
// started by a timer, for example, runs outside the domain.
//
// Counting costs one atomic add per allocation inside a domain, and a
// load and a branch per allocation outside one.
//
// The domain counts cumulative allocation, not live memory: bytes are
// never subtracted when objects are freed.
type AllocDomain struct {
	d unsafe.Pointer
}

// NewAllocDomain returns a new domain with a count of zero.
func NewAllocDomain() *AllocDomain {
	return &AllocDomain{d: runtime_newAllocDomain()}
}

// Run calls f on the calling goroutine with its allocations counted in
// d, then restores the goroutine's previous domain, if any. Goroutines f
// starts are in d for their whole lives. Run may be nested; the
// innermost domain counts.
func (d *AllocDomain) Run(f func()) {
	prev := runtime_setAllocDomain(d.d)
	defer runtime_setAllocDomain(prev)
	f()
}

// Allocated returns the number of bytes allocated in d so far.
func (d *AllocDomain) Allocated() uint64 {
	return runtime_allocDomainTotal(d.d)
}

//go:linkname runtime_newAllocDomain
func runtime_newAllocDomain() unsafe.Pointer

//go:linkname runtime_setAllocDomain
func runtime_setAllocDomain(d unsafe.Pointer) (prev unsafe.Pointer)

//go:linkname runtime_allocDomainTotal
func runtime_allocDomainTotal(d unsafe.Pointer) uint64
