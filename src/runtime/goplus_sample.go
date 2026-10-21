// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Sampling tags (goplus.SampleTag): estimate the live heap bytes of a
// group of goroutines from the heap profiler's allocation samples.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// goplusSampleTag holds the live-bytes estimate of the goroutines whose
// g.sampleTag points to it.
//
// Each allocation the heap profiler samples on such a goroutine adds its
// weight to live and records the tag and the weight in the sample's
// specialprofile; freeing the object subtracts the same weight. So live is
// always the sum of the weights of the tag's sampled objects not yet
// swept free.
//
// A specialprofile is not in the heap, so markrootSpans treats its tag as
// a root: a tag lives as long as any of its samples.
type goplusSampleTag struct {
	live    atomic.Int64
	samples atomic.Int64 // samples not yet swept free; for tests
}

// goplusSampleWeight returns the bytes one sampled allocation of size
// bytes stands for when the sampler takes one allocation per rate bytes
// on average: size divided by the probability that it was sampled, the
// scaling runtime/pprof applies to heap samples (scaleHeapSample). At a
// rate of 1 every allocation is sampled and the weight is its size.
//
// The sampler draws at most every maxSampleMean bytes on average, however
// high the rate (see fastexprand), so a higher rate is weighted as that.
func goplusSampleWeight(size uintptr, rate int) int64 {
	if rate <= 1 {
		return int64(size)
	}
	rate = min(rate, maxSampleMean)
	x := float64(size) / float64(rate)
	return int64(float64(size)/goplusOneMinusExpNeg(x) + 0.5)
}

// goplusOneMinusExpNeg returns 1-e^(-x) for x > 0, to a relative error
// far below the sampling error it is used to correct. The runtime has no
// math.Exp.
func goplusOneMinusExpNeg(x float64) float64 {
	if x < 1.0/1024 {
		// The series, which keeps the precision 1-e^(-x) loses to
		// cancellation for small x.
		return x * (1 - x/2*(1-x/3*(1-x/4)))
	}
	if x > 40 {
		return 1 // e^(-40) is below float64's resolution at 1
	}
	// e^(-x) = (e^(-x/2^n))^(2^n), with x/2^n small enough for a short
	// Taylor series.
	n := 0
	for x > 1.0/64 {
		x /= 2
		n++
	}
	e := 1 - x*(1-x/2*(1-x/3*(1-x/4*(1-x/5*(1-x/6)))))
	for ; n > 0; n-- {
		e *= e
	}
	return 1 - e
}

// goplusSampleAlloc is called by mProf_Malloc for each sampled allocation,
// with the rate the sample was drawn at. It returns the allocating
// goroutine's tag and the sample's weight, which the caller records in the
// sample's specialprofile, or nil if the goroutine has no tag.
func goplusSampleAlloc(mp *m, size uintptr, rate int) (*goplusSampleTag, int64) {
	gp := mp.curg
	if gp == nil || gp.sampleTag == nil {
		return nil, 0
	}
	t := gp.sampleTag
	w := goplusSampleWeight(size, rate)
	t.live.Add(w)
	t.samples.Add(1)
	return t, w
}

//go:linkname goplus_newSampleTag goplus.runtime_newSampleTag
func goplus_newSampleTag() unsafe.Pointer {
	return unsafe.Pointer(new(goplusSampleTag))
}

// goplus_setSampleTag gives the calling goroutine tag t (nil for none) and
// returns its previous tag. newproc1 copies it to new goroutines.
//
//go:linkname goplus_setSampleTag goplus.runtime_setSampleTag
func goplus_setSampleTag(t unsafe.Pointer) (prev unsafe.Pointer) {
	gp := getg()
	prev = unsafe.Pointer(gp.sampleTag)
	gp.sampleTag = (*goplusSampleTag)(t)
	return prev
}

//go:linkname goplus_sampleTagLive goplus.runtime_sampleTagLive
func goplus_sampleTagLive(t unsafe.Pointer) uint64 {
	v := (*goplusSampleTag)(t).live.Load()
	if v < 0 {
		// Weights are added before the sample is recorded and subtracted
		// only when it is freed, so this cannot happen; report it rather
		// than wrap.
		throw("goplus: negative sample tag estimate")
	}
	return uint64(v)
}
