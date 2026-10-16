// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"runtime"
	"sync"
	"testing"
)

// allocSinks keeps allocations reachable, one slot per worker so that
// concurrent workers do not write the same variable. The slots are typed:
// storing a slice in an interface would allocate its header too.
var allocSinks [64][]byte

// allocExact allocates n pointer-free objects of size bytes, as worker w.
// Sizes used with it are exact size classes (or whole pages, for large
// objects), so the bytes requested equal the bytes the allocator hands
// out, and it allocates nothing else.
//
//go:noinline
func allocExact(w, n, size int) {
	for range n {
		allocSinks[w] = make([]byte, size)
	}
}

// TestAllocDomainMatchesMemStats compares a single domain's count with
// the growth of MemStats.TotalAlloc over the same work.
func TestAllocDomainMatchesMemStats(t *testing.T) {
	d := goplus.NewAllocDomain()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d.Run(func() {
		allocExact(0, 20000, 64)
		allocExact(0, 2000, 4096)
		allocExact(0, 20, 1<<20)
	})
	runtime.ReadMemStats(&after)
	const want = 20000*64 + 2000*4096 + 20*(1<<20)
	got := d.Allocated()
	total := after.TotalAlloc - before.TotalAlloc
	t.Logf("domain counted %d bytes; TotalAlloc grew by %d; the work requested %d", got, total, want)
	// The domain counts the work plus whatever else this goroutine
	// allocated meanwhile, which is small.
	if got < want || got > want+want/100 {
		t.Errorf("domain counted %d bytes, want %d within 1%%", got, want)
	}
	// TotalAlloc also counts other goroutines' allocations; with exact
	// size classes the domain's share must match it to within 1%.
	if diff := int64(total) - int64(got); diff < -int64(want/100) || diff > int64(want/100) {
		t.Errorf("TotalAlloc grew by %d, domain counted %d: they differ by more than 1%%", total, got)
	}
}

func TestAllocDomainOutside(t *testing.T) {
	d := goplus.NewAllocDomain()
	allocExact(0, 1000, 64)
	done := make(chan bool)
	go func() { allocExact(1, 1000, 64); done <- true }()
	<-done
	if n := d.Allocated(); n != 0 {
		t.Fatalf("domain counted %d bytes for allocations made outside it", n)
	}
	d.Run(func() {})
	allocExact(0, 1000, 64) // after Run returned: outside again
	if n := d.Allocated(); n > 4096 {
		t.Fatalf("domain counted %d bytes after Run returned", n)
	}
}

// TestAllocDomainInherited checks that goroutines started in a domain
// count in it, even when they allocate after Run has returned, and that
// their own children inherit it too.
func TestAllocDomainInherited(t *testing.T) {
	d := goplus.NewAllocDomain()
	release := make(chan bool)
	done := make(chan bool)
	d.Run(func() {
		go func() {
			<-release
			allocExact(1, 1000, 4096)
			go func() {
				allocExact(2, 1000, 4096)
				done <- true
			}()
		}()
	})
	if n := d.Allocated(); n > 4096 {
		t.Fatalf("domain counted %d bytes before its goroutine allocated", n)
	}
	close(release)
	<-done
	if n, want := d.Allocated(), uint64(2*1000*4096); n < want {
		t.Fatalf("domain counted %d bytes, want at least %d from its goroutine and grandchild", n, want)
	}
}

func TestAllocDomainNested(t *testing.T) {
	outer, inner := goplus.NewAllocDomain(), goplus.NewAllocDomain()
	outer.Run(func() {
		allocExact(0, 1000, 64)
		inner.Run(func() { allocExact(0, 1000, 4096) })
		allocExact(0, 1000, 64)
	})
	if n := inner.Allocated(); n < 1000*4096 || n > 1000*4096+4096 {
		t.Errorf("inner domain counted %d bytes, want about %d", n, 1000*4096)
	}
	if n := outer.Allocated(); n < 2*1000*64 || n >= 1000*4096 {
		t.Errorf("outer domain counted %d bytes, want about %d and none of the inner work", n, 2*1000*64)
	}
}

// TestAllocDomainConcurrent allocates from many goroutines of one domain
// on every P and checks the total.
func TestAllocDomainConcurrent(t *testing.T) {
	const workers, perWorker, size = 32, 2000, 128
	d := goplus.NewAllocDomain()
	var wg sync.WaitGroup
	d.Run(func() {
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allocExact(w, perWorker, size)
			}()
		}
	})
	wg.Wait()
	want := uint64(workers * perWorker * size)
	if n := d.Allocated(); n < want || n > want+want/100 {
		t.Fatalf("domain counted %d bytes, want %d within 1%%", n, want)
	}
}

// TestAllocDomainReusedGoroutine checks that a goroutine started outside
// any domain is not counted even when it reuses the record of a goroutine
// that ran, and exited, inside one. Goroutine records go on a per-P free
// list when a goroutine exits and the next goroutine started on that P
// takes one from it, so starting goroutines on one P right after a batch
// exited makes reuse near certain.
func TestAllocDomainReusedGoroutine(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const n = 64
	d := goplus.NewAllocDomain()
	var wg sync.WaitGroup
	d.Run(func() {
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allocExact(1, 10, 64)
			}()
		}
	})
	wg.Wait()
	runtime.Gosched() // let the exited goroutines' records reach the free list
	before := d.Allocated()
	if before < n*10*64 {
		t.Fatalf("domain counted %d bytes from its own goroutines, want at least %d", before, n*10*64)
	}
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allocExact(2, 10, 4096)
		}()
	}
	wg.Wait()
	if after := d.Allocated(); after != before {
		t.Fatalf("domain grew from %d to %d bytes from goroutines started outside it (stale domain on a reused goroutine record)", before, after)
	}
}

// BenchmarkAllocDomain measures an allocation inside a domain against
// one outside.
func BenchmarkAllocDomain(b *testing.B) {
	b.Run("outside", func(b *testing.B) {
		for b.Loop() {
			allocSinks[0] = make([]byte, 64)
		}
	})
	b.Run("inside", func(b *testing.B) {
		goplus.NewAllocDomain().Run(func() {
			for b.Loop() {
				allocSinks[0] = make([]byte, 64)
			}
		})
	})
}
