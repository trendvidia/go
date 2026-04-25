// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Cross-format dispatch interfaces and pickers.
//
// Where Phase 1 left a `switch ctxt.HeadType` in shared orchestration
// files calling per-format helpers with matching signatures, Phase 2
// promotes the dispatch to a small interface satisfied by per-format
// types declared in this file. The picker that selects the
// implementation runs once per orchestration call; after that, all
// dispatch is method-call.
//
// New interfaces here remain unexported and must not leak into the
// public ld package surface.

package ld

import (
	"cmd/internal/objabi"
	"cmd/link/internal/sym"
)

// segmentLayout computes the file offset of a segment placed after prev
// during (*Link).layout. Implementations encode each format's rules for
// segment file packing and rounding.
type segmentLayout interface {
	segmentFileOffset(seg, prev *sym.Segment) uint64
}

// defaultSegmentLayout implements ELF/Macho/XCOFF/Wasm segment packing:
// round prev's tail to *FlagRound and verify the (Vaddr%R == Fileoff%R)
// invariant required so segment loads land at aligned virtual
// addresses.
type defaultSegmentLayout struct{}

func (defaultSegmentLayout) segmentFileOffset(seg, prev *sym.Segment) uint64 {
	fileoff := uint64(Rnd(int64(prev.Fileoff+prev.Filelen), *FlagRound))
	if seg.Vaddr%uint64(*FlagRound) != fileoff%uint64(*FlagRound) {
		Exitf("bad segment rounding (Vaddr=%#x Fileoff=%#x FlagRound=%#x)", seg.Vaddr, fileoff, *FlagRound)
	}
	return fileoff
}

// peSegmentLayout dispatches to peSegmentFileOffset (pe.go).
type peSegmentLayout struct{}

func (peSegmentLayout) segmentFileOffset(seg, prev *sym.Segment) uint64 {
	return peSegmentFileOffset(prev)
}

// plan9SegmentLayout dispatches to plan9SegmentFileOffset (plan9.go).
type plan9SegmentLayout struct{}

func (plan9SegmentLayout) segmentFileOffset(seg, prev *sym.Segment) uint64 {
	return plan9SegmentFileOffset(prev)
}

// pickSegmentLayout returns the segment-layout implementation for
// ctxt.HeadType. All implementations are zero-sized struct values, so
// this allocates nothing.
func pickSegmentLayout(ctxt *Link) segmentLayout {
	switch ctxt.HeadType {
	case objabi.Hwindows:
		return peSegmentLayout{}
	case objabi.Hplan9:
		return plan9SegmentLayout{}
	default:
		return defaultSegmentLayout{}
	}
}

// hostlinkState carries the cross-cutting variables that hostlink's
// per-format argv-extension steps read and write. It exists so that the
// hostlinkExtender interface can have a uniform single-argument shape
// even though Macho needs combineDwarf as input and PE needs to write
// back isMSVC/wlPrefix. Other formats touch only argv.
//
// hostlink's downstream code (ASLR flag synthesis, export-dynamic
// switch, mingw libs, combineDwarf-gated post-link processing) reads
// these values back as locals after dispatch, so the struct does not
// extend beyond the argv-assembly step.
type hostlinkState struct {
	argv         []string
	combineDwarf bool
	isMSVC       bool
	wlPrefix     string
}

// hostlinkExtender appends format-specific flags to a hostlink
// invocation. Implementations mutate st.argv (and, for PE, also
// st.isMSVC and st.wlPrefix). Linux/FreeBSD/etc. have no per-format
// extensions and are represented by a nil return from
// pickHostlinkExtender — callers must guard the dispatch.
type hostlinkExtender interface {
	hostlinkArgs(ctxt *Link, st *hostlinkState)
}

type machoHostlink struct{}

func (machoHostlink) hostlinkArgs(ctxt *Link, st *hostlinkState) {
	st.argv = machoArgs(ctxt, st.argv, st.combineDwarf)
}

type openbsdHostlink struct{}

func (openbsdHostlink) hostlinkArgs(ctxt *Link, st *hostlinkState) {
	st.argv = openbsdArgs(ctxt, st.argv)
}

type peHostlink struct{}

func (peHostlink) hostlinkArgs(ctxt *Link, st *hostlinkState) {
	st.argv, st.isMSVC, st.wlPrefix = peArgs(ctxt, st.argv)
}

type xcoffHostlink struct{}

func (xcoffHostlink) hostlinkArgs(ctxt *Link, st *hostlinkState) {
	st.argv = xcoffArgs(ctxt, st.argv)
}

// pickHostlinkExtender returns the per-format extender for ctxt.HeadType
// or nil if no extension applies (Linux/FreeBSD/Plan9/etc.). Callers
// must check the result before invoking the method.
func pickHostlinkExtender(ctxt *Link) hostlinkExtender {
	switch ctxt.HeadType {
	case objabi.Hdarwin:
		return machoHostlink{}
	case objabi.Hopenbsd:
		return openbsdHostlink{}
	case objabi.Hwindows:
		return peHostlink{}
	case objabi.Haix:
		return xcoffHostlink{}
	}
	return nil
}
