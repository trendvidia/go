// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Runtime -> tracer API for memory events.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"internal/runtime/sys"
	"internal/trace/tracev2"
	"unsafe"
)

// Batch type values for the alloc/free experiment.
const (
	traceAllocFreeTypesBatch = iota // Contains types. [{id, address, size, ptrspan, name length, name string} ...]
	traceAllocFreeInfoBatch         // Contains info for interpreting events. [min heap addr, page size, min heap align, min stack align]
)

// traceSnapshotMemory takes a snapshot of all runtime memory that there are events for
// (heap spans, heap objects, goroutine stacks, etc.) and writes out events for them.
//
// The world must be stopped and tracing must be enabled when this function is called.
func traceSnapshotMemory(gen uintptr) {
	assertWorldStopped()

	// Write a batch containing information that'll be necessary to
	// interpret the events.
	var flushed bool
	w := unsafeTraceExpWriter(gen, nil, tracev2.AllocFree)
	w, flushed = w.ensure(1 + 4*traceBytesPerNumber)
	if flushed {
		// Annotate the batch as containing additional info.
		w.byte(byte(traceAllocFreeInfoBatch))
	}

	// Emit info.
	w.varint(trace.minPageHeapAddr)
	w.varint(uint64(pageSize))
	w.varint(uint64(gc.MinHeapAlign))
	w.varint(uint64(fixedStack))

	// Finish writing the batch.
	w.flush().end()

	// Start tracing.
	trace := traceAcquire()
	if !trace.ok() {
		throw("traceSnapshotMemory: tracing is not enabled")
	}

	// Write out all the heap spans and heap objects.
	for _, s := range mheap_.allspans {
		if s.state.get() == mSpanDead {
			continue
		}
		// It's some kind of span, so trace that it exists.
		trace.SpanExists(s)

		// Write out allocated objects if it's a heap span.
		if s.state.get() != mSpanInUse {
			continue
		}

		// Find all allocated objects.
		abits := s.allocBitsForIndex(0)
		for i := uintptr(0); i < uintptr(s.nelems); i++ {
			if abits.index < uintptr(s.freeindex) || abits.isMarked() {
				x := s.base() + i*s.elemsize
				trace.HeapObjectExists(x, traceRecordedType(s, i, x))
			}
			abits.advance()
		}
	}

	// Write out all the goroutine stacks.
	forEachGRace(func(gp *g) {
		trace.GoroutineStackExists(gp.stack.lo, gp.stack.hi-gp.stack.lo)
	})
	traceRelease(trace)
}

// traceRecordedType returns the type the allocator recorded for object
// i at x in span s, its malloc header or its span's largeType, or nil if
// none is recorded. It returns nil without reading the object for a dead
// object in a span not yet swept: its type may be a goplus.Layout type
// that died in the same cycle, already freed and reused
// (trendvidia/go#63). Any other allocated object is live, and a live
// object keeps its type alive (see shadeObjectType), so the type it
// returns may be read.
//
// The world is stopped, so no span is part way through being swept.
func traceRecordedType(s *mspan, i, x uintptr) *abi.Type {
	if s.spanclass.noscan() || heapBitsInSpan(s.elemsize) {
		return nil
	}
	if sg := mheap_.sweepgen; s.sweepgen == sg-2 || s.sweepgen == sg+1 {
		// Unswept: the mark bits are this cycle's, and an allocated
		// object without one is dead.
		if !s.markBitsForIndex(i).isMarked() {
			return nil
		}
	}
	if s.spanclass.sizeclass() != 0 {
		return *(**abi.Type)(unsafe.Pointer(x))
	}
	return (*abi.Type)(atomic.Loadp(unsafe.Pointer(&s.largeType)))
}

// traceTypeFor returns the type to record for an object of type typ in
// an alloc/free event: typ itself, or nil for a goplus.Layout type. The
// trace's type table holds types outside the collector's view until the
// generation ends, and reads them then; a layout type can be freed
// before that (trendvidia/go#63). Every other type, the types reflect
// makes included, is never freed. Events already carry no type for
// every object the allocator records none for, so consumers handle its
// absence.
//
// typ must be live: it is read.
func traceTypeFor(typ *abi.Type) *abi.Type {
	if typ != nil && isLayoutType(typ) {
		return nil
	}
	return typ
}

func traceSpanTypeAndClass(s *mspan) traceArg {
	if s.state.get() == mSpanInUse {
		return traceArg(s.spanclass) << 1
	}
	return traceArg(1)
}

// SpanExists records an event indicating that the span exists.
func (tl traceLocker) SpanExists(s *mspan) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvSpan, traceSpanID(s), traceArg(s.npages), traceSpanTypeAndClass(s))
}

// SpanAlloc records an event indicating that the span has just been allocated.
func (tl traceLocker) SpanAlloc(s *mspan) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvSpanAlloc, traceSpanID(s), traceArg(s.npages), traceSpanTypeAndClass(s))
}

// SpanFree records an event indicating that the span is about to be freed.
func (tl traceLocker) SpanFree(s *mspan) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvSpanFree, traceSpanID(s))
}

// traceSpanID creates a trace ID for the span s for the trace.
func traceSpanID(s *mspan) traceArg {
	return traceArg(uint64(s.base())-trace.minPageHeapAddr) / pageSize
}

// HeapObjectExists records that an object already exists at addr with the provided type.
// The type is optional, and the size of the slot occupied the object is inferred from the
// span containing it.
func (tl traceLocker) HeapObjectExists(addr uintptr, typ *abi.Type) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvHeapObject, traceHeapObjectID(addr), tl.rtype(traceTypeFor(typ)))
}

// HeapObjectAlloc records that an object was newly allocated at addr with the provided type.
// The type is optional, and the size of the slot occupied the object is inferred from the
// span containing it.
func (tl traceLocker) HeapObjectAlloc(addr uintptr, typ *abi.Type) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvHeapObjectAlloc, traceHeapObjectID(addr), tl.rtype(traceTypeFor(typ)))
}

// HeapObjectFree records that an object at addr is about to be freed.
func (tl traceLocker) HeapObjectFree(addr uintptr) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvHeapObjectFree, traceHeapObjectID(addr))
}

// traceHeapObjectID creates a trace ID for a heap object at address addr.
func traceHeapObjectID(addr uintptr) traceArg {
	return traceArg(uint64(addr)-trace.minPageHeapAddr) / gc.MinHeapAlign
}

// GoroutineStackExists records that a goroutine stack already exists at address base with the provided size.
func (tl traceLocker) GoroutineStackExists(base, size uintptr) {
	order := traceCompressStackSize(size)
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvGoroutineStack, traceGoroutineStackID(base), order)
}

// GoroutineStackAlloc records that a goroutine stack was newly allocated at address base with the provided size..
func (tl traceLocker) GoroutineStackAlloc(base, size uintptr) {
	order := traceCompressStackSize(size)
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvGoroutineStackAlloc, traceGoroutineStackID(base), order)
}

// GoroutineStackFree records that a goroutine stack at address base is about to be freed.
func (tl traceLocker) GoroutineStackFree(base uintptr) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvGoroutineStackFree, traceGoroutineStackID(base))
}

// traceGoroutineStackID creates a trace ID for the goroutine stack from its base address.
func traceGoroutineStackID(base uintptr) traceArg {
	return traceArg(uint64(base)-trace.minPageHeapAddr) / fixedStack
}

// traceCompressStackSize assumes size is a power of 2 and returns log2(size).
func traceCompressStackSize(size uintptr) traceArg {
	if size&(size-1) != 0 {
		throw("goroutine stack size is not a power of 2")
	}
	return traceArg(sys.Len64(uint64(size)))
}
