// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"goplus"
	"internal/abi"
	"io"
	"runtime"
	"runtime/metrics"
	"runtime/trace"
	"unsafe"
)

func init() {
	register("GoplusLayoutTraceAllocFree", GoplusLayoutTraceAllocFree)
}

// GoplusLayoutTraceAllocFree creates and drops goplus.Layout types while
// an execution trace runs, for GODEBUG=traceallocfree=1
// (trendvidia/go#63). The alloc/free trace keeps the types it sees in a
// table the collector does not scan and reads them when a generation
// ends, and its starting snapshot reads the type of every allocated
// slot, dead ones in unswept spans included; a freed layout type there
// is reused memory.
func GoplusLayoutTraceAllocFree() {
	const ptrSize = unsafe.Sizeof(uintptr(0))
	churn := func() {
		var objs []unsafe.Pointer
		for range 1000 {
			objs = append(objs, goplus.NewLayout(1024, ptrSize, []byte{1}).New())
		}
		runtime.KeepAlive(objs)
		objs = nil
		runtime.GC()
		// Reuse the freed types' slots.
		var junk [][]uintptr
		for range 5000 {
			b := make([]uintptr, 10)
			for j := range b {
				b[j] = 0xdeadbeef
			}
			junk = append(junk, b)
		}
		runtime.KeepAlive(junk)
	}

	// One long trace: types enter the table, die, and are dumped when
	// the generation ends.
	if err := trace.Start(io.Discard); err != nil {
		panic(err)
	}
	for range 50 {
		churn()
	}
	trace.Stop()

	// Many short traces, each starting with a snapshot taken while dead
	// layout objects sit in spans not yet swept and their types' slots
	// are already reused. A collection that allocation triggers sweeps
	// lazily, one size class at a time: allocating blocks of the type
	// descriptor's size class sweeps and refills those spans first, and
	// leaves the objects' spans (another class) unswept.
	// With one P the background sweeper runs only when this goroutine
	// yields, so the objects' spans stay unswept until the snapshot.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	// A large live heap, so a collection leaves many spans to sweep.
	var ballast [][]*int
	for range 50_000 {
		ballast = append(ballast, make([]*int, 64))
	}
	defer runtime.KeepAlive(ballast)
	cycles := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	for range 50 {
		var objs []unsafe.Pointer
		for range 1000 {
			objs = append(objs, goplus.NewLayout(1024, ptrSize, []byte{1}).New())
		}
		runtime.KeepAlive(objs)
		objs = nil
		metrics.Read(cycles)
		start := cycles[0].Value.Uint64()
		var junk []*traceJunk
		for i := 0; ; i++ {
			junk = append(junk, newTraceJunk())
			if i%1024 == 0 {
				metrics.Read(cycles)
				if cycles[0].Value.Uint64() > start {
					break
				}
				if len(junk) > 1<<16 {
					junk = junk[:0]
				}
			}
		}
		for range 2000 {
			junk = append(junk, newTraceJunk())
		}
		if err := trace.Start(io.Discard); err != nil {
			panic(err)
		}
		trace.Stop()
		runtime.KeepAlive(junk)
	}
	println("TRACE-DONE")
}

// traceJunk is the size of a layout's type descriptor (an abi.StructType)
// and, like it, holds a pointer, so it is allocated from the same spans
// and reuses a freed descriptor's slot. Every other word is 0xdeadbeef,
// the descriptor's mask pointer among them.
type traceJunk struct {
	p *int
	v [unsafe.Sizeof(abi.StructType{})/unsafe.Sizeof(uintptr(0)) - 1]uintptr
}

func newTraceJunk() *traceJunk {
	j := new(traceJunk)
	for i := range j.v {
		j.v[i] = 0xdeadbeef
	}
	return j
}
