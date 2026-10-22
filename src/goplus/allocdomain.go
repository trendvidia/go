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
//
// A domain can also refuse allocations past a budget, but only those a
// goroutine makes while it is inside a call to [AllocDomain.Run]; see
// [AllocDomain.SetBudget].
//
// A domain must be created with [NewAllocDomain]; the zero value is not
// a domain, and its methods panic.
type AllocDomain struct {
	d unsafe.Pointer
}

// NewAllocDomain returns a new domain with a count of zero and no budget.
func NewAllocDomain() *AllocDomain {
	return &AllocDomain{d: runtime_newAllocDomain()}
}

// Run calls f on the calling goroutine with its allocations counted in
// d, then restores the goroutine's previous domain, if any. Goroutines f
// starts are in d for their whole lives. Run may be nested; the
// innermost domain counts.
//
// While f runs, d's budget may refuse the calling goroutine's
// allocations. It never refuses those of a goroutine f starts, whether
// or not Run has returned: that goroutine is counted in d but is not
// inside the call, and a refusal would panic where no caller of Run can
// recover it. A goroutine of d that later calls Run itself, on d or on
// another domain, is inside that call until it returns, and the budget
// of the domain it runs in may refuse it there.
func (d *AllocDomain) Run(f func()) {
	// A nil domain would take the goroutine out of any enclosing one.
	prev, prevInRun := runtime_setAllocDomain(d.domain(), true)
	defer runtime_setAllocDomain(prev, prevInRun)
	f()
}

// Allocated returns the number of bytes allocated in d so far.
func (d *AllocDomain) Allocated() uint64 {
	return runtime_allocDomainTotal(d.domain())
}

// ErrAllocBudget is the value of the panic that refuses an allocation
// past its domain's budget. It is a [runtime.Error].
var ErrAllocBudget error = runtime_allocBudgetError()

// SetBudget sets the most d may allocate, in the bytes [AllocDomain.Allocated]
// counts. [math.MaxUint64] removes the budget, and is the budget of a new
// domain. 0 is a budget with nothing left: d's next checked allocation is
// refused. (Before [JIT] level 10, 0 removed the budget.) It may be called
// at any time, including while goroutines of d run, to grant more or less.
//
// An allocation that takes d past its budget, made by a goroutine while
// it is inside a call to d.Run, is refused: the allocation panics with
// [ErrAllocBudget], which the goroutine can recover, and is not counted.
// Goroutines of d that are not inside a call to Run, such as those a
// call started, are counted and never refused, so the budget does not
// bound them; [AllocDomain.Allocated] still includes what they allocate.
// (Before [JIT] level 11, the budget refused every goroutine of d.)
//
// A single large request inside Run is refused before any memory is
// requested from the operating system, so a call such as strings.Repeat
// that asks for more than the machine has is refused rather than fatal.
// The same call in a goroutine that is not inside Run is let through,
// and is fatal. Small allocations are checked against the budget each
// time d allocates another 64 KiB on a P, and the first checked one made
// inside Run after d is past its budget is refused. While only
// goroutines inside Run allocate, d can pass its budget by up to 64 KiB
// times the number of Ps they allocate on before one is refused; the
// goroutines outside Run can take it past by any amount.
//
// An allocation that cannot panic is let through and counted, and the
// next one that can is refused instead. These are allocations the
// runtime makes on a system stack or holding a lock (when starting a
// goroutine, for example), and those a map makes while it is being
// assigned to, which would leave the map unusable if they panicked.
//
// A refusal panics at an allocation, a place where Go code does not
// expect a panic. Code that holds a lock, or is part-way through
// updating a data structure, when it allocates is left as any panic
// leaves it, and a [sync.Once] whose function is refused is done. A
// goroutine that recovers from ErrAllocBudget should treat the state it
// was changing as lost.
func (d *AllocDomain) SetBudget(n uint64) {
	runtime_setAllocDomainBudget(d.domain(), n)
}

func (d *AllocDomain) domain() unsafe.Pointer {
	if d.d == nil {
		panic("goplus: AllocDomain not created by NewAllocDomain")
	}
	return d.d
}

//go:linkname runtime_newAllocDomain
func runtime_newAllocDomain() unsafe.Pointer

//go:linkname runtime_setAllocDomain
func runtime_setAllocDomain(d unsafe.Pointer, inRun bool) (prev unsafe.Pointer, prevInRun bool)

//go:linkname runtime_allocDomainTotal
func runtime_allocDomainTotal(d unsafe.Pointer) uint64

//go:linkname runtime_setAllocDomainBudget
func runtime_setAllocDomainBudget(d unsafe.Pointer, n uint64)

//go:linkname runtime_allocBudgetError
func runtime_allocBudgetError() error
