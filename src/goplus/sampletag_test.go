// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// tagSamples returns the number of t's samples not yet swept free.
func tagSamples(t *goplus.SampleTag) int64 {
	return (*[2]int64)(goplus.SampleTagState(t))[1]
}

// withMemProfileRate runs the test at sampling rate r.
func withMemProfileRate(t *testing.T, r int) {
	prev := runtime.MemProfileRate
	runtime.MemProfileRate = r
	t.Cleanup(func() { runtime.MemProfileRate = prev })
}

// keepMixed allocates about total bytes in a mix of small and large
// sizes, all exact size classes or whole pages so the bytes the heap
// profile records are the bytes requested, and returns them with their
// exact total. Each quarter of the bytes comes from one size. The total
// includes keep's own backing array, a large object of the requested size.
func keepMixed(total int) (keep [][]byte, bytes int) {
	sizes := []int{64, 1024, 32 << 10, 1 << 20}
	n := 0
	for _, s := range sizes {
		n += total / 4 / s
	}
	keep = make([][]byte, 0, n)
	bytes = n * int(unsafe.Sizeof(keep[0]))
	for _, s := range sizes {
		for range total / 4 / s {
			keep = append(keep, make([]byte, s))
			bytes += s
		}
	}
	return keep, bytes
}

// gcUntil runs collections until cond holds, failing the test after a
// deadline: a free reaches a tag only when the collector sweeps it.
func gcUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.GC()
	}
}

// TestSampleTagLive checks the estimate against the bytes a tag's
// goroutines keep live and then free, while an untagged goroutine
// allocates as much alongside them.
func TestSampleTagLive(t *testing.T) {
	// At 4 KiB this mix of 64 MiB takes about 8k samples (large objects
	// are sampled once however large), and the estimate's standard
	// deviation is about 0.5%, most of it from the smallest sizes. The 5%
	// bound is ten of them.
	withMemProfileRate(t, 4096)
	const total = 64 << 20

	tag := goplus.NewSampleTag()
	var tagged, untagged [][]byte
	var bytes int
	var wg sync.WaitGroup
	wg.Add(2)
	tag.Run(func() {
		// A goroutine started inside Run is in the tag.
		go func() {
			defer wg.Done()
			tagged, bytes = keepMixed(total)
		}()
	})
	go func() {
		defer wg.Done()
		untagged, _ = keepMixed(total)
	}()
	wg.Wait()

	live, samples := tag.Live(), tagSamples(tag)
	t.Logf("live estimate %d for %d bytes kept (%+.2f%%), from %d samples",
		live, bytes, 100*(float64(live)-float64(bytes))/float64(bytes), samples)
	if samples < 1000 {
		t.Fatalf("%d samples, want at least 1000: is the profiler sampling?", samples)
	}
	if lo, hi := uint64(bytes)*95/100, uint64(bytes)*105/100; live < lo || live > hi {
		t.Errorf("live estimate %d, want within 5%% of %d", live, bytes)
	}

	runtime.KeepAlive(tagged)
	tagged = nil
	gcUntil(t, "the tag's samples to be swept", func() bool { return tagSamples(tag) < samples/100 })
	if live := tag.Live(); live > uint64(bytes)/100 {
		t.Errorf("live estimate %d after freeing, want under 1%% of the %d bytes freed", live, bytes)
	}
	runtime.KeepAlive(untagged)
}

// TestSampleTagNested checks that the innermost tag counts, and that
// leaving Run leaves the tag.
func TestSampleTagNested(t *testing.T) {
	withMemProfileRate(t, 1) // every allocation
	outer, inner := goplus.NewSampleTag(), goplus.NewSampleTag()
	var a, b, c []byte
	outer.Run(func() {
		a = make([]byte, 1<<20)
		inner.Run(func() { b = make([]byte, 2<<20) })
	})
	c = make([]byte, 4<<20)
	// Other allocations inside Run, such as defer records, are counted
	// too; at rate 1 they are only a few bytes each.
	if l := outer.Live(); l < 1<<20 || l >= 2<<20 {
		t.Errorf("outer estimate %d, want 1 MiB and change", l)
	}
	if l := inner.Live(); l < 2<<20 || l >= 3<<20 {
		t.Errorf("inner estimate %d, want 2 MiB and change", l)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	runtime.KeepAlive(c)
}

// TestSampleTagOutlivesHandle checks that a tag stays alive while any of
// its samples does, even once nothing else refers to it: the free of a
// sample subtracts from the tag, so collecting it first would be a
// write to freed memory. The runtime keeps it alive by treating the
// sample's profile record, which is not in the heap, as a GC root.
func TestSampleTagOutlivesHandle(t *testing.T) {
	withMemProfileRate(t, 1)
	keep, wp := func() ([]byte, weak.Pointer[[2]int64]) {
		tag := goplus.NewSampleTag()
		var keep []byte
		tag.Run(func() { keep = make([]byte, 1<<20) })
		return keep, weak.Make((*[2]int64)(goplus.SampleTagState(tag)))
	}()
	for range 5 {
		runtime.GC()
	}
	s := wp.Value()
	if s == nil {
		t.Fatal("tag collected while a sample of it was live")
	}
	if s[1] == 0 {
		t.Fatal("tag has no samples: the test proves nothing")
	}
	s = nil
	// The positive half: with the sample freed, the tag is collectable.
	runtime.KeepAlive(keep)
	keep = nil
	gcUntil(t, "the tag to be collected", func() bool { return wp.Value() == nil })
}

// TestSampleTagNotSampling checks that a tag reads 0 when the profiler
// samples nothing.
func TestSampleTagNotSampling(t *testing.T) {
	withMemProfileRate(t, 0)
	onOneP(func() {
		sink := make([]byte, 64) // takes the sample a rate change forces
		runtime.KeepAlive(sink)
		tag := goplus.NewSampleTag()
		var keep []byte
		tag.Run(func() {
			for range 64 {
				keep = make([]byte, 1<<20)
			}
		})
		if l := tag.Live(); l != 0 {
			t.Errorf("estimate %d at MemProfileRate 0, want 0", l)
		}
		runtime.KeepAlive(keep)
	})
}

// TestSampleTagRateChange checks the sample the allocator takes of the
// first allocation after MemProfileRate changes, to pick up the new rate.
// That allocation is sampled with certainty, so it stands for exactly its
// own size: weighting it at either rate would bias the estimate, and
// leaving it out would lose a large object the sampler would have taken
// anyway.
func TestSampleTagRateChange(t *testing.T) {
	withMemProfileRate(t, 1<<30) // samples a 1 MiB object 1 time in 1000
	onOneP(func() {
		sink := make([]byte, 64) // takes the sample this change forces
		runtime.KeepAlive(sink)
		tag := goplus.NewSampleTag()
		runtime.MemProfileRate = 1 << 29 // the cleanup restores the test's rate
		var keep []byte
		tag.Run(func() { keep = make([]byte, 1<<20) })
		if l := tag.Live(); l != 1<<20 {
			t.Errorf("estimate %d after a rate change, want the first allocation's size, %d", l, 1<<20)
		}
		runtime.KeepAlive(keep)
	})
}

func TestSampleTagZeroValue(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Live on the zero SampleTag did not panic")
		}
	}()
	new(goplus.SampleTag).Live()
}
