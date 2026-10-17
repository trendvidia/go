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
				trace.HeapObjectExists(x, traceRecordedType(s, x))
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

// traceRecordedType returns the type the allocator recorded for the
// object at x in span s: its malloc header, or its span's largeType; nil
// if no type is recorded with the object. It never dereferences the
// type. The snapshot visits dead objects in spans not yet swept, and the
// type of such an object may be a goplus.Layout type that died in the
// same cycle and whose memory is already reused (trendvidia/go#63).
func traceRecordedType(s *mspan, x uintptr) *abi.Type {
	if s.spanclass.noscan() || heapBitsInSpan(s.elemsize) {
		return nil
	}
	if s.spanclass.sizeclass() != 0 {
		return *(**abi.Type)(unsafe.Pointer(x))
	}
	return (*abi.Type)(atomic.Loadp(unsafe.Pointer(&s.largeType)))
}

// traceHeapTypeless drops a type made at run time, one in the heap, from
// an alloc/free event. The trace's type table holds types outside the
// collector's view until the generation ends, and dumps them then; a
// goplus.Layout type can be freed before that (trendvidia/go#63).
// Types reflect makes are in the heap too and lose their type the same
// way. Events already carry no type for every object the allocator
// records none for, so consumers handle its absence.
//
// The test is the address alone, in a heap arena or not, so it holds for
// a type already freed, whatever has become of its span.
func traceHeapTypeless(typ *abi.Type) *abi.Type {
	if typ != nil && heapArenaOf(uintptr(unsafe.Pointer(typ))) != nil {
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
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvHeapObject, traceHeapObjectID(addr), tl.rtype(traceHeapTypeless(typ)))
}

// HeapObjectAlloc records that an object was newly allocated at addr with the provided type.
// The type is optional, and the size of the slot occupied the object is inferred from the
// span containing it.
func (tl traceLocker) HeapObjectAlloc(addr uintptr, typ *abi.Type) {
	tl.eventWriter(tracev2.GoRunning, tracev2.ProcRunning).event(tracev2.EvHeapObjectAlloc, traceHeapObjectID(addr), tl.rtype(traceHeapTypeless(typ)))
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
