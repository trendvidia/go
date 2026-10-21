// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "unsafe"

// A SampleTag estimates the live heap bytes of a group of goroutines:
// those that run code inside [SampleTag.Run], and every goroutine they
// start, directly or indirectly, including goroutines that outlive the
// call to Run.
//
// The estimate comes from the heap profiler's allocation sampling, the
// samples [runtime.MemProfile] reports. Each allocation the profiler
// samples on a goroutine of the tag adds the bytes it stands for, its
// size divided by the chance it was sampled, as runtime/pprof scales a
// heap profile. Freeing the object subtracts them again. Sizes are those
// the heap profile records: a small object's size class, a large
// object's requested size.
//
// The estimate is unbiased, and its error falls as the number of live
// samples grows: its relative standard deviation is about
// 1/√(live bytes / [runtime.MemProfileRate]). At the default rate of
// 512 KiB that is about 18% for 16 MiB live and 3% for 1 GiB. The rate
// is process-wide: lowering it makes the estimate finer and costs a
// stack capture at every sample, on every goroutine of the process, not
// only on those of a tag. At a rate of 0 nothing is sampled and the
// estimate stays 0.
//
// The tiny allocator packs pointer-free allocations under 16 bytes, from
// any goroutine on the same P, into shared 16-byte blocks, and the
// profiler samples a block as a whole. A sampled block counts toward the
// goroutine that started it, and stays counted while any allocation in
// it lives, whoever made that one. With [SetNoTinyAlloc], each such
// allocation of a goroutine gets a slot of its own, counted toward it alone.
//
// A free is subtracted when the collector sweeps the object, not when
// the last reference to it is dropped. So between collections the
// estimate also counts the tag's garbage: it reads high, never low.
//
// A tag is independent of an [AllocDomain]. It never refuses an
// allocation, and the goroutines of a tag can enter and leave domains
// without leaving the tag. Allocations the runtime makes on another
// goroutine on the tag's behalf are not counted: a finalizer, a cleanup,
// or a function started by a timer, for example, runs outside the tag.
//
// Tagging costs nothing on an allocation the profiler does not sample,
// and two atomic adds to the tag on one it does, at the sample and again
// at the free.
//
// A tag must be created with [NewSampleTag]; the zero value is not a
// tag, and its methods panic.
type SampleTag struct {
	t unsafe.Pointer
}

// NewSampleTag returns a new tag with an estimate of zero.
//
// A program that creates a tag keeps the heap profiler sampling at
// [runtime.MemProfileRate]. Without one, the linker turns sampling off
// in a program that never reads the memory profile.
func NewSampleTag() *SampleTag {
	return &SampleTag{t: runtime_newSampleTag()}
}

// Run calls f on the calling goroutine with its sampled allocations
// counted toward t, then restores the goroutine's previous tag, if any.
// Goroutines f starts are in t for their whole lives. Run may be nested;
// the innermost tag counts.
func (t *SampleTag) Run(f func()) {
	prev := runtime_setSampleTag(t.tag())
	defer runtime_setSampleTag(prev)
	f()
}

// Live returns t's current estimate of its goroutines' live heap bytes.
func (t *SampleTag) Live() uint64 {
	return runtime_sampleTagLive(t.tag())
}

func (t *SampleTag) tag() unsafe.Pointer {
	if t.t == nil {
		panic("goplus: SampleTag not created by NewSampleTag")
	}
	return t.t
}

//go:linkname runtime_newSampleTag
func runtime_newSampleTag() unsafe.Pointer

//go:linkname runtime_setSampleTag
func runtime_setSampleTag(t unsafe.Pointer) (prev unsafe.Pointer)

//go:linkname runtime_sampleTagLive
func runtime_sampleTagLive(t unsafe.Pointer) uint64
