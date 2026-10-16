// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"goplus/internal/execmem"
	"internal/testenv"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func mapCode(t *testing.T, code []byte) *execmem.Code {
	t.Helper()
	if !execmem.Supported() || code == nil {
		t.Skipf("no generated-code support on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	c, err := execmem.Map(code)
	if err != nil {
		t.Fatalf("mapping code: %v", err)
	}
	t.Cleanup(func() { c.Unmap() })
	return c
}

func TestWriteBarrierEntryRange(t *testing.T) {
	for n := 1; n <= 8; n++ {
		if goplus.WriteBarrierEntry(n) == 0 {
			t.Errorf("WriteBarrierEntry(%d) = 0", n)
		}
	}
	for _, n := range []int{0, 9, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WriteBarrierEntry(%d) did not panic", n)
				}
			}()
			goplus.WriteBarrierEntry(n)
		}()
	}
}

// TestWriteBarrierFlag checks that the flag reads nonzero while a
// collection is marking and zero when none is running.
func TestWriteBarrierFlag(t *testing.T) {
	flag := goplus.WriteBarrierFlag()
	runtime.GC()
	if v := *flag; v != 0 {
		t.Fatalf("flag = %#x with no collection running", v)
	}
	// Keep collections running and sample the flag until it is seen set.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if *flag != 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("flag never read nonzero while collections ran for 10s")
}

// TestWriteBarrierEntryPreservesRegisters calls the entry from generated
// code often enough to overflow the write-barrier buffer, so it takes the
// flush path as well as the fast one, and checks that every register the
// contract promises to preserve still holds what the code put there.
func TestWriteBarrierEntryPreservesRegisters(t *testing.T) {
	// The buffer holds 512 pointers; each call reserves 2.
	const calls = 300
	c := mapCode(t, probeCode(goplus.WriteBarrierEntry(2), calls))
	probe := execmem.Func[func() uintptr](c)
	for i := range 20 {
		if got := probe(); got != 0 {
			t.Fatalf("call %d: a preserved register changed across the write-barrier entry (xor mask %#x)", i, got)
		}
	}
}

type wbNode struct {
	id, check uint64
	_         [2]uint64 // 32 bytes: past the tiny allocator
}

func newNode(id uint64) unsafe.Pointer {
	return unsafe.Pointer(&wbNode{id: id, check: ^id})
}

// TestWriteBarrierStress moves pointers between heap slots using
// generated code that records each store through the exported barrier,
// while collections run back to back, then checks that no object reached
// only through those slots was freed and reused.
//
// The stores swap pointers between two arrays, so each one overwrites a
// pointer that may be the object's last reference from the heap and
// installs one the collector may already have passed. That is the case
// the barrier exists for. Fresh objects would not test it: objects
// allocated during marking are allocated marked.
func TestWriteBarrierStress(t *testing.T) {
	c := mapCode(t, storeCode(uintptr(unsafe.Pointer(goplus.WriteBarrierFlag())), goplus.WriteBarrierEntry(2), true))
	store := execmem.Func[func(dst *unsafe.Pointer, val unsafe.Pointer)](c)

	defer debug.SetGCPercent(debug.SetGCPercent(1))
	// Run for a number of collections, not of swaps: what matters is how
	// many marking phases the stores overlap.
	cycles := 100
	if testing.Short() {
		cycles = 25
	}
	const slots = 4096
	workers := min(runtime.GOMAXPROCS(0), 8)

	stop := make(chan struct{})
	start := make(chan struct{})
	go func() {
		<-start
		for range cycles {
			runtime.GC()
		}
		close(stop)
	}()

	var wg sync.WaitGroup
	errs := make(chan string, workers)
	swaps := make([]int, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 1))
			a := make([]unsafe.Pointer, slots)
			b := make([]unsafe.Pointer, slots)
			next := uint64(w) << 40
			for i := range a {
				a[i], b[i] = newNode(next), newNode(next+1)
				next += 2
			}
			var garbage [][4]uint64
			ready.Done()
			for n := 0; ; n++ {
				if n%256 == 0 {
					select {
					case <-stop:
						swaps[w] = n
						goto verify
					default:
					}
				}
				i, j := r.IntN(slots), r.IntN(slots)
				tmp := a[i]
				store(&a[i], b[j])
				store(&b[j], tmp)
				if n%64 == 0 {
					// Replace a slot, and allocate same-sized garbage so a
					// wrongly freed node is soon overwritten.
					store(&a[r.IntN(slots)], newNode(next))
					next++
					garbage = append(garbage, [4]uint64{0xdead, 0xbeef, 0xdead, 0xbeef})
					if len(garbage) > 256 {
						garbage = nil
					}
				}
			}
		verify:
			for _, s := range [][]unsafe.Pointer{a, b} {
				for k, p := range s {
					if n := (*wbNode)(p); n.check != ^n.id {
						errs <- "slot " + strconv.Itoa(k) + " holds a freed and reused object"
						return
					}
				}
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// A positive marker: every worker kept swapping across the collections.
	total := 0
	for w, n := range swaps {
		if n < cycles*100 {
			t.Fatalf("worker %d made only %d swaps across %d collections; it did not test the barrier", w, n, cycles)
		}
		total += n
	}
	t.Logf("%d workers, %d swaps across %d collections", workers, total, cycles)
}

// TestWriteBarrierCheckmark reruns the stress with GODEBUG=gccheckmark=1,
// under which the runtime re-marks the heap with the world stopped after
// each cycle and throws if the concurrent mark missed a reachable object:
// it detects a missing barrier directly rather than through reuse.
func TestWriteBarrierCheckmark(t *testing.T) {
	if os.Getenv("GOPLUS_WB_CHILD") != "" {
		t.Skip("in child")
	}
	if !execmem.Supported() {
		t.Skipf("no generated-code support on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestWriteBarrierStress$", "-test.v", "-test.short")
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1", "GOPLUS_WB_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestWriteBarrierStress") {
		t.Fatalf("stress under gccheckmark=1 failed: %v\n%s", err, out)
	}
}

// storePtr is the compiled-Go fallback for a pointer store from generated
// code: the compiler emits the write barrier. It is nosplit, so it has no
// safe point at which the runtime could walk the generated frame below
// it.
//
//go:nosplit
//go:noinline
func storePtr(dst *unsafe.Pointer, val unsafe.Pointer) { *dst = val }

// BenchmarkWriteBarrierStore compares, with no collection running, the
// cost per pointer store from generated code three ways: with no barrier
// (the baseline), with the exported barrier's inline flag test, and
// through a call to a compiled Go helper. Each call makes storesPerCall
// stores in a loop inside the generated code, so the cost of entering it
// from Go is spread thin; the metric is ns per store.
func BenchmarkWriteBarrierStore(b *testing.B) {
	if !execmem.Supported() || storeLoopCode(loopPlain, 0, 0, 0) == nil {
		b.Skipf("no generated-code support on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const storesPerCall = 1000
	flag := uintptr(unsafe.Pointer(goplus.WriteBarrierFlag()))
	entry := goplus.WriteBarrierEntry(2)
	helper := reflect.ValueOf(storePtr).Pointer()
	for _, bc := range []struct {
		name string
		mode int
	}{{"plain", loopPlain}, {"inline", loopInline}, {"helper", loopHelper}} {
		c, err := execmem.Map(storeLoopCode(bc.mode, flag, entry, helper))
		if err != nil {
			b.Fatal(err)
		}
		storeN := execmem.Func[func(dst *unsafe.Pointer, val unsafe.Pointer, n int)](c)
		slot := new(unsafe.Pointer)
		val := unsafe.Pointer(new(wbNode))
		// Check the loop stores before timing it.
		storeN(slot, val, 3)
		if *slot != val {
			b.Fatalf("%s: loop did not store", bc.name)
		}
		runtime.GC()
		b.Run(bc.name, func(b *testing.B) {
			for b.Loop() {
				storeN(slot, val, storesPerCall)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*storesPerCall), "ns/store")
		})
		c.Unmap()
	}
}
