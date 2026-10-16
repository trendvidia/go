// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import (
	"errors"
	"internal/goarch"
	"slices"
	"sync"
	"unsafe"
)

// A Func describes one function of generated code to the runtime, so that
// a goroutine stack holding a frame of it can be scanned by the garbage
// collector, grown (copied) and traced back like one holding compiled Go
// frames.
//
// The function must keep Go's frame layout for its architecture:
//
//   - amd64: the return address is just above the frame. A function with
//     a frame saves the caller's BP in the frame's top word and sets BP to
//     its address, as compiled Go code does with
//     SUBQ $size, SP; MOVQ BP, size-8(SP); LEAQ size-8(SP), BP.
//   - arm64: a function with a frame saves LR at 0(SP), saves the caller's
//     frame pointer at -8(SP) (below its frame) and sets R29 to SP-8, as
//     compiled Go code does with STR LR, [SP, #-size]!; STUR R29,
//     [SP, #-8]; SUB R29, SP, #8. The frame's top word is the caller's and
//     must not be written. Frame sizes are multiples of 16.
//
// Its arguments arrive in registers; stack-passed arguments are not
// supported. When it calls a Go function, it must reserve, at the bottom
// of its frame, the spill space the callee's register arguments need
// under ABIInternal (see the package documentation).
type Func struct {
	Name       string
	Entry, End uintptr // the code, [Entry, End)

	// Frame gives the frame size, in bytes below the stack pointer at
	// entry, from each code offset on, sorted by Off. Frame[0].Off is 0.
	// It must be exact at every instruction: a signal can arrive anywhere
	// and the runtime unwinds from it.
	Frame []FrameStep

	// SafePoints lists every place the runtime may find this frame below
	// another one: each call the function makes. A frame is never stopped
	// inside the function itself, because no point of it is safe for
	// asynchronous preemption.
	SafePoints []SafePoint

	// File and Lines, both optional, give the source position tracebacks
	// print for a PC.
	File  string
	Lines []LineStep
}

// A FrameStep gives the frame size from code offset Off on.
type FrameStep struct {
	Off  uint32 // offset from Func.Entry
	Size uint32 // bytes below the entry stack pointer
}

// A SafePoint describes the frame at one call.
type SafePoint struct {
	// Off is the offset of the call's return address from Func.Entry.
	Off uint32

	// Pointers lists the stack-pointer offsets of the frame words that
	// hold live pointers during the call. The collector keeps what they
	// point to alive, and a stack copy adjusts those that point into the
	// stack. Words not listed are not scanned.
	Pointers []uint32
}

// A LineStep gives the source line from code offset Off on.
type LineStep struct {
	Off  uint32
	Line int32
}

// A CodeRange is a set of registered functions.
type CodeRange struct {
	mu sync.Mutex
	h  unsafe.Pointer // nil once unregistered
}

// RegisterCode makes the functions known to the runtime. They must not
// overlap each other, compiled code, or other registered code. The
// descriptions are copied; funcs can be reused once RegisterCode returns.
//
// RegisterCode is supported on amd64 and arm64.
func RegisterCode(funcs []Func) (*CodeRange, error) {
	t, err := encodeFuncs(funcs)
	if err != nil {
		return nil, err
	}
	h, msg := runtime_registerCode(t.minpc, t.maxpc, t.funcs, t.pctab, t.names, t.files, t.cutab, t.gofunc)
	if msg != "" {
		return nil, errors.New("goplus: RegisterCode: " + msg)
	}
	return &CodeRange{h: h}, nil
}

// Unregister removes the functions from the runtime. It stops the world
// once, and fails, leaving them registered, if any goroutine stack holds
// a frame of them. After it returns nil, the code can be freed, and a
// runtime.Func obtained for one of its PCs is no longer valid.
func (r *CodeRange) Unregister() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.h == nil {
		return errors.New("goplus: Unregister: not registered")
	}
	if msg := runtime_unregisterCode(r.h); msg != "" {
		return errors.New("goplus: Unregister: " + msg)
	}
	r.h = nil
	return nil
}

//go:linkname runtime_registerCode
func runtime_registerCode(minpc, maxpc uintptr, funcs []uint32, pctab, funcnametab, filetab []byte, cutab []uint32, gofunc []byte) (unsafe.Pointer, string)

//go:linkname runtime_unregisterCode
func runtime_unregisterCode(h unsafe.Pointer) string

// Per-function record layout shared with runtime/goplus_code.go.
const (
	recEntryOff = iota
	recNameOff
	recPCSP
	recPCFile
	recPCLn
	recPCUnsafePoint
	recPCStackMap
	recLocalsMap
	recStartLine
	recWords
)

const (
	unsafePointUnsafe = -2 // abi.UnsafePointUnsafe
)

type tables struct {
	minpc, maxpc uintptr
	funcs        []uint32
	pctab        []byte
	names, files []byte
	cutab        []uint32
	gofunc       []byte
}

func encodeFuncs(in []Func) (*tables, error) {
	switch goarch.GOARCH {
	case "amd64", "arm64":
	default:
		return nil, errors.New("goplus: RegisterCode: unsupported architecture")
	}
	if len(in) == 0 {
		return nil, errors.New("goplus: RegisterCode: no functions")
	}
	funcs := slices.Clone(in)
	slices.SortFunc(funcs, func(a, b Func) int {
		switch {
		case a.Entry < b.Entry:
			return -1
		case a.Entry > b.Entry:
			return 1
		}
		return 0
	})
	t := &tables{
		minpc: funcs[0].Entry,
		maxpc: funcs[len(funcs)-1].End,
		pctab: []byte{0}, // offset 0 means "no table"
		names: []byte{0}, // name offset 0 means "no name"
	}
	if t.maxpc <= t.minpc || t.maxpc-t.minpc >= 1<<31 {
		return nil, errors.New("goplus: RegisterCode: code range empty or too large")
	}
	fileIndex := map[string]int32{}
	for i := range funcs {
		f := &funcs[i]
		if i > 0 && funcs[i-1].End > f.Entry {
			return nil, funcErr(f, "overlaps "+funcs[i-1].Name)
		}
		rec, err := t.encodeFunc(f, fileIndex)
		if err != nil {
			return nil, err
		}
		t.funcs = append(t.funcs, rec[:]...)
	}
	return t, nil
}

func funcErr(f *Func, msg string) error {
	return errors.New("goplus: RegisterCode: " + f.Name + ": " + msg)
}

func (t *tables) encodeFunc(f *Func, fileIndex map[string]int32) ([recWords]uint32, error) {
	var rec [recWords]uint32
	const q = goarch.PCQuantum
	frameAlign := uint32(goarch.PtrSize)
	if goarch.GOARCH == "arm64" {
		frameAlign = 16
	}
	if f.Entry >= f.End || f.Entry%q != 0 || f.End%q != 0 {
		return rec, funcErr(f, "bad code range")
	}
	size := uint32(f.End - f.Entry)
	if len(f.Frame) == 0 || f.Frame[0].Off != 0 {
		return rec, funcErr(f, "Frame must start at offset 0")
	}
	frame := make([]step, len(f.Frame))
	for i, s := range f.Frame {
		if i > 0 && s.Off <= f.Frame[i-1].Off || s.Off >= size || s.Off%q != 0 {
			return rec, funcErr(f, "Frame offsets must be increasing, in the code, and instruction-aligned")
		}
		if s.Size%frameAlign != 0 {
			return rec, funcErr(f, "frame size not aligned")
		}
		frame[i] = step{s.Off, int32(s.Size)}
	}
	frameAt := func(off uint32) uint32 {
		v := f.Frame[0].Size
		for _, s := range f.Frame {
			if s.Off > off {
				break
			}
			v = s.Size
		}
		return v
	}

	// Stack maps: index 0 has no pointers and covers every PC that is
	// not a declared safe point; safe point i uses index i+1. The words
	// a bitmap covers end just below varp, the saved frame pointer word,
	// which is size-8 above SP on both architectures.
	nbit := uint32(0)
	maxBits := ^uint32(0)
	for i, sp := range f.SafePoints {
		if i > 0 && sp.Off <= f.SafePoints[i-1].Off || sp.Off == 0 || sp.Off > size || sp.Off%q != 0 {
			return rec, funcErr(f, "safe points must be increasing return-address offsets within the code")
		}
		fs := frameAt(sp.Off - q)
		if fs == 0 {
			if len(sp.Pointers) > 0 {
				return rec, funcErr(f, "pointers at a safe point with no frame")
			}
			continue
		}
		varp := fs - 8
		maxBits = min(maxBits, varp/8)
		for _, o := range sp.Pointers {
			if o%8 != 0 || o >= varp || goarch.GOARCH == "arm64" && o < 8 {
				return rec, funcErr(f, "pointer offset outside the frame's locals")
			}
			nbit = max(nbit, (varp-o)/8)
		}
	}
	if maxBits != ^uint32(0) && nbit > maxBits {
		return rec, funcErr(f, "pointer offsets do not fit every safe point's frame")
	}
	nmaps := uint32(len(f.SafePoints) + 1)
	bytesPer := (nbit + 7) / 8
	for len(t.gofunc)%4 != 0 {
		t.gofunc = append(t.gofunc, 0)
	}
	rec[recLocalsMap] = uint32(len(t.gofunc))
	t.gofunc = appendU32(t.gofunc, nmaps)
	t.gofunc = appendU32(t.gofunc, nbit)
	t.gofunc = append(t.gofunc, make([]byte, bytesPer)...) // index 0: none
	for _, sp := range f.SafePoints {
		bm := make([]byte, bytesPer)
		if fs := frameAt(sp.Off - q); fs > 0 {
			base := fs - 8 - nbit*8 // SP offset of bit 0
			for _, o := range sp.Pointers {
				j := (o - base) / 8
				bm[j/8] |= 1 << (j % 8)
			}
		}
		t.gofunc = append(t.gofunc, bm...)
	}

	var smap []step
	at := uint32(0)
	for i, sp := range f.SafePoints {
		if sp.Off-q > at {
			smap = append(smap, step{at, -1})
		}
		smap = append(smap, step{sp.Off - q, int32(i + 1)})
		at = sp.Off
	}
	if at < size {
		smap = append(smap, step{at, -1})
	}

	rec[recEntryOff] = uint32(f.Entry - t.minpc)
	rec[recNameOff] = uint32(len(t.names))
	t.names = append(append(t.names, f.Name...), 0)
	rec[recPCSP] = t.addTable(frame, size)
	rec[recPCUnsafePoint] = t.addTable([]step{{0, unsafePointUnsafe}}, size)
	rec[recPCStackMap] = t.addTable(smap, size)
	if f.File != "" {
		idx, ok := fileIndex[f.File]
		if !ok {
			idx = int32(len(t.cutab))
			fileIndex[f.File] = idx
			t.cutab = append(t.cutab, uint32(len(t.files)))
			t.files = append(append(t.files, f.File...), 0)
		}
		rec[recPCFile] = t.addTable([]step{{0, idx}}, size)
	}
	if len(f.Lines) > 0 {
		lines := make([]step, 0, len(f.Lines)+1)
		if f.Lines[0].Off != 0 {
			lines = append(lines, step{0, f.Lines[0].Line})
		}
		for i, l := range f.Lines {
			if i > 0 && l.Off <= f.Lines[i-1].Off || l.Off >= size || l.Off%q != 0 {
				return rec, funcErr(f, "Lines offsets must be increasing, in the code, and instruction-aligned")
			}
			lines = append(lines, step{l.Off, l.Line})
		}
		rec[recPCLn] = t.addTable(lines, size)
		rec[recStartLine] = uint32(f.Lines[0].Line)
	}
	return rec, nil
}

type step struct {
	off uint32
	val int32
}

// addTable encodes steps, each value holding from its offset to the next
// step's (the last to size), in the runtime's pc-value table format, and
// returns the table's offset in pctab.
func (t *tables) addTable(steps []step, size uint32) uint32 {
	const q = goarch.PCQuantum
	off := uint32(len(t.pctab))
	prev := int32(-1)
	first := true
	for i := 0; i < len(steps); {
		// Merge runs of the same value: a zero value delta ends a table.
		j := i + 1
		for j < len(steps) && steps[j].val == steps[i].val {
			j++
		}
		end := size
		if j < len(steps) {
			end = steps[j].off
		}
		d := steps[i].val - prev
		if d == 0 && !first {
			panic("goplus: unmerged pc-value step")
		}
		t.pctab = appendUvarint(t.pctab, uint32(d<<1)^uint32(d>>31))
		t.pctab = appendUvarint(t.pctab, (end-steps[i].off)/q)
		prev = steps[i].val
		first = false
		i = j
	}
	t.pctab = append(t.pctab, 0) // end of table
	return off
}

func appendUvarint(b []byte, v uint32) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
