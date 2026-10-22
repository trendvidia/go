// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Allocation domains (goplus.AllocDomain): count the heap bytes allocated
// by a group of goroutines, and refuse allocations past a budget.

package runtime

import (
	"internal/cpu"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"internal/stringslite"
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

	// budget is the most the domain may allocate, or goplusNoBudget.
	budget atomic.Uint64
}

// goplusNoBudget is the budget of a domain with no limit, math.MaxUint64.
// A budget of 0 is a limit like any other: nothing may be allocated.
const goplusNoBudget = ^uint64(0)

// goplusBudgetGranule is how often a stripe checks the domain's budget:
// each time its count crosses a multiple of the granule, and on every
// allocation at least as large. Summing the stripes is too dear for
// every small allocation, so the domain can pass its budget by less than
// one granule per stripe before an allocation is refused.
const goplusBudgetGranule = 64 << 10

// add counts n bytes allocated by the current goroutine and reports
// whether the allocation must be refused because it takes the domain past
// its budget; a refused allocation is not counted, and the caller panics
// with goplusAllocBudgetError once it holds none of its own locks. held
// is the number of m.locks the caller itself holds at the call.
//
// Allocation calls add only when the goroutine is in a domain, so it is
// kept out of line.
//
//go:noinline
func (d *goplusAllocDomain) add(n uintptr, held int32) bool {
	i := 0
	if pp := getg().m.p.ptr(); pp != nil {
		i = int(pp.id) & (len(d.stripes) - 1)
	}
	s := &d.stripes[i].n
	v := s.Add(int64(n))
	if d.budget.Load() == goplusNoBudget {
		return false
	}
	if n < goplusBudgetGranule && (v-uint64(n))/goplusBudgetGranule == v/goplusBudgetGranule {
		return false
	}
	return d.refuse(s, n, held)
}

// refuse is add's budget check, for an allocation of n bytes already
// counted in stripe s. It uncounts and refuses the allocation when the
// domain is over budget and the goroutine can take a panic.
//
//go:noinline
func (d *goplusAllocDomain) refuse(s *atomic.Uint64, n uintptr, held int32) bool {
	// An allocation that cannot panic is allowed, and stays counted: the
	// domain's next allocation that can panic is refused instead. So is
	// one by a goroutine whose panic would not reach a caller of Run (see
	// goplusRefusable). These are checked before the stripes are summed,
	// which they make pointless.
	gp := getg()
	mp := gp.m
	if gp != mp.curg || mp.locks != held || mp.mallocing != 0 || mp.preemptoff != "" || mp.dying != 0 || !goplusRefusable(gp, d) {
		return false
	}
	budget := d.budget.Load()
	if budget == goplusNoBudget || goplus_allocDomainTotal(unsafe.Pointer(d)) <= budget {
		return false
	}
	if goplusInMapWrite() {
		return false
	}
	s.Add(-int64(n))
	return true
}

// goplusRefusable reports whether d may refuse an allocation by gp, a
// goroutine counted in d: whether the panic would unwind out of a call
// to d.Run, where the code that called Run can recover it.
//
// That holds for a goroutine inside a Run call of its own. It does not
// for one that Run's function started: nothing around that goroutine
// recovers, and a panic there would end the process (trendvidia/go#84).
// A coroutine, the goroutine an iter.Pull iterator runs on, is a third
// case (trendvidia/go#88). It runs only while the goroutine that called
// next or stop waits for it, and iter.Pull hands its panics to that
// goroutine, so it may be refused when that goroutine may be refused in
// d. The waiting goroutine is held in the coro until the coroutine
// switches back, and may itself be a coroutine.
func goplusRefusable(gp *g, d *goplusAllocDomain) bool {
	// Each step is a coroutine and the goroutine waiting on it. Only a
	// misused coro can make the chain loop, which the bound stops; a
	// chain longer than any recursion of iter.Pull reaches is left
	// unrefused.
	for range 1 << 16 {
		if gp.goplusInRun {
			return gp.allocDomain == d
		}
		c := gp.goplusCoro
		if c == nil {
			return false
		}
		gp = c.gp.ptr()
		if gp == nil {
			return false
		}
	}
	return false
}

// goplusInMapWrite reports whether the current goroutine is allocating
// inside a map assignment. A map marks itself written for the whole of an
// assignment, its growth included, and a panic there would leave it
// marked: its next write would throw "concurrent map writes". Each
// allocation a map makes is bounded by its maximum table size, so letting
// these through cannot admit one huge request.
//
// The runtime's frames between the allocator and the code that called
// into it say whether the allocation came from a map assignment.
func goplusInMapWrite() bool {
	pc, sp, gp := sys.GetCallerPC(), sys.GetCallerSP(), getg()
	inMap := false
	systemstack(func() {
		var u unwinder
		u.initAt(pc, sp, 0, gp, unwindSilentErrors)
		for ; u.valid(); u.next() {
			for iu, uf := newInlineUnwinder(u.frame.fn, u.symPC()); uf.valid(); uf = iu.next(uf) {
				name := iu.srcFunc(uf).name()
				if stringslite.HasPrefix(name, "runtime.mapassign") {
					inMap = true
					return
				}
				if !stringslite.HasPrefix(name, "runtime.") && !stringslite.HasPrefix(name, "internal/runtime/") {
					return
				}
			}
		}
	})
	return inMap
}

// goplusAllocBudgetError is the panic value of an allocation refused by
// its domain's budget; goplus exports it as ErrAllocBudget. It has no
// fields, so making it an interface value does not allocate.
type goplusAllocBudgetError struct{}

func (goplusAllocBudgetError) RuntimeError() {}

func (goplusAllocBudgetError) Error() string {
	return "runtime error: allocation exceeds the goplus.AllocDomain budget"
}

// goplusPanicAllocBudget raises the panic for a refused allocation.
//
//go:noinline
func goplusPanicAllocBudget() {
	panic(goplusAllocBudgetError{})
}

//go:linkname goplus_allocBudgetError goplus.runtime_allocBudgetError
func goplus_allocBudgetError() error {
	return goplusAllocBudgetError{}
}

//go:linkname goplus_newAllocDomain goplus.runtime_newAllocDomain
func goplus_newAllocDomain() unsafe.Pointer {
	d := new(goplusAllocDomain)
	d.budget.Store(goplusNoBudget)
	return unsafe.Pointer(d)
}

// goplus_setAllocDomain puts the calling goroutine in domain d (nil for
// none), marks whether it is inside a Run call, and returns what it
// replaced. newproc1 copies the domain to the goroutines it starts, but
// not the mark.
//
//go:linkname goplus_setAllocDomain goplus.runtime_setAllocDomain
func goplus_setAllocDomain(d unsafe.Pointer, inRun bool) (prev unsafe.Pointer, prevInRun bool) {
	gp := getg()
	prev, prevInRun = unsafe.Pointer(gp.allocDomain), gp.goplusInRun
	gp.allocDomain = (*goplusAllocDomain)(d)
	gp.goplusInRun = inRun
	return prev, prevInRun
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

//go:linkname goplus_setAllocDomainBudget goplus.runtime_setAllocDomainBudget
func goplus_setAllocDomainBudget(d unsafe.Pointer, n uint64) {
	(*goplusAllocDomain)(d).budget.Store(n)
}
