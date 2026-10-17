// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"goplus"
	"internal/abi"
	"internal/runtime/gc"
	itrace "internal/trace"
	"io"
	"os"
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
	// Every kind of layout object: with a malloc header, and without
	// one, small or pointer-free. Each one's alloc event names its type.
	churn := func() {
		var objs []unsafe.Pointer
		for range 1000 {
			objs = append(objs,
				goplus.NewLayout(1024, ptrSize, []byte{1}).New(),
				goplus.NewLayout(64, ptrSize, []byte{1}).New(),
				goplus.NewLayout(1024, ptrSize, nil).New())
		}
		runtime.KeepAlive(objs)
		objs = nil
		runtime.GC()
		// Reuse the freed types' slots.
		var junk []*traceJunk
		for range 5000 {
			junk = append(junk, newTraceJunk())
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
	//
	// Each trace is parsed to show that happened: the snapshot records
	// dead layout objects with no type, some of them with their type's
	// slot holding junk. A dead object's slot can be reused before the
	// snapshot, so they are counted rather than required of every object;
	// the slices are allocated up front to make that rare.
	// (TestTraceAllocFreeTypes checks the types the snapshot keeps.)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	// A large live heap, so a collection leaves many spans to sweep.
	var ballast [][]*int
	for range 50_000 {
		ballast = append(ballast, make([]*int, 64))
	}
	defer runtime.KeepAlive(ballast)
	cycles := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	var layoutSeen, reusedSeen int
	objs := make([]unsafe.Pointer, 0, 1000)
	junk := make([]*traceJunk, 0, 1<<16+4096)
	for range 50 {
		// The slot of each dead object, which starts with its malloc
		// header, and the address of its type, read from that header.
		deadLayout := map[uintptr]uintptr{}
		for range 1000 {
			p := goplus.NewLayout(1024, ptrSize, []byte{1}).New()
			objs = append(objs, p)
			slot := unsafe.Add(p, -gc.MallocHeaderSize)
			deadLayout[uintptr(slot)] = *(*uintptr)(slot)
		}
		clear(objs)
		objs = objs[:0]
		metrics.Read(cycles)
		start := cycles[0].Value.Uint64()
		junk = junk[:0]
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
		var buf bytes.Buffer
		if err := trace.Start(&buf); err != nil {
			panic(err)
		}
		trace.Stop()
		junkAt := map[uintptr]bool{}
		for _, j := range junk {
			junkAt[uintptr(unsafe.Pointer(j))] = true
		}
		clear(junk)

		for _, o := range traceSnapshotObjects(&buf) {
			if typ, ok := deadLayout[o.addr]; ok && o.typ == 0 {
				layoutSeen++
				if junkAt[typ] {
					reusedSeen++
				}
			}
		}
	}
	// The staging above depends on how the runtime sweeps; without these,
	// TRACE-DONE would say nothing about the snapshot.
	if layoutSeen == 0 || reusedSeen == 0 {
		fail("snapshots saw %d dead layout objects with no type, %d of them with their type's slot reused; want each > 0", layoutSeen, reusedSeen)
	}
	println("dead layout objects", layoutSeen, "types reused", reusedSeen)
	println("TRACE-DONE")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}

type traceHeapObject struct {
	addr uintptr
	typ  uint64
}

// traceSnapshotObjects parses a trace and returns the heap objects its
// alloc/free snapshot records, by slot address and type id.
func traceSnapshotObjects(r io.Reader) []traceHeapObject {
	tr, err := itrace.NewReader(r)
	if err != nil {
		panic(err)
	}
	var minAddr, align uint64
	var ids []traceHeapObject
	for {
		ev, err := tr.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		switch ev.Kind() {
		case itrace.EventSync:
			// The info batch: a batch type of 1, then the minimum heap
			// address, the page size and the heap's minimum alignment.
			for _, b := range ev.Sync().ExperimentalBatches["AllocFree"] {
				if len(b.Data) == 0 || b.Data[0] != 1 {
					continue
				}
				d := b.Data[1:]
				var v [3]uint64
				for i := range v {
					x, n := binary.Uvarint(d)
					if n <= 0 {
						panic("bad alloc/free info batch")
					}
					v[i], d = x, d[n:]
				}
				minAddr, align = v[0], v[2]
			}
		case itrace.EventExperimental:
			e := ev.Experimental()
			if e.Name == "HeapObject" {
				ids = append(ids, traceHeapObject{uintptr(e.ArgValue(0).Uint64()), e.ArgValue(1).Uint64()})
			}
		}
	}
	if align == 0 {
		panic("trace has no alloc/free info batch")
	}
	for i := range ids {
		ids[i].addr = uintptr(uint64(ids[i].addr)*align + minAddr)
	}
	return ids
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
