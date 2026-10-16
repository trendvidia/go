// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Registration of code generated at run time (goplus.RegisterCode).
//
// Each registration becomes a synthetic moduledata holding the same
// tables the linker writes for compiled code (pcsp, pcdata, stack maps,
// names), so findfunc, the unwinder, GC stack scanning and stack copying
// handle generated frames unchanged. These modules are kept off the
// firstmoduledata chain: they hold no data, bss, types or itabs, and the
// plugin code assumes that chain grows by exactly one node per load.
// findmoduledatap consults them only after the chain misses.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

// goplusCodeSet is an immutable, sorted, non-overlapping set of
// registered code modules. Readers load the current one without
// locking. Registering publishes a new set with compare-and-swap;
// unregistering replaces it with the world stopped, so a registration
// built against the old set fails its swap and retries.
type goplusCodeSet struct {
	mods []*moduledata // sorted by minpc
}

var goplusCode atomic.Pointer[goplusCodeSet]

// goplusFindModule returns the registered code module containing pc, or
// nil.
//
// It is nosplit because findmoduledatap is.
//
//go:nosplit
func goplusFindModule(pc uintptr) *moduledata {
	set := goplusCode.Load()
	if set == nil {
		return nil
	}
	mods := set.mods
	lo, hi := 0, len(mods)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if mods[m].maxpc <= pc {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(mods) && mods[lo].minpc <= pc {
		return mods[lo]
	}
	return nil
}

// goplusModuleOfFunc returns the registered code module whose function
// table holds the _func at address ptr, or nil.
func goplusModuleOfFunc(ptr uintptr) *moduledata {
	set := goplusCode.Load()
	if set == nil {
		return nil
	}
	for _, md := range set.mods {
		base := uintptr(unsafe.Pointer(&md.pclntable[0]))
		if base <= ptr && ptr < base+uintptr(len(md.pclntable)) {
			return md
		}
	}
	return nil
}

// Layout of the per-function records goplus passes in, in uint32 words.
const (
	goplusFuncEntryOff = iota
	goplusFuncNameOff
	goplusFuncPCSP
	goplusFuncPCFile
	goplusFuncPCLn
	goplusFuncPCUnsafePoint
	goplusFuncPCStackMap
	goplusFuncLocalsMap // offset into gofunc of the locals stackmap
	goplusFuncStartLine
	goplusFuncWords
)

// goplusCodeHeader is placed at the start of each registration's
// allocation.
type goplusCodeHeader struct {
	md   moduledata
	size uintptr // of the whole allocation
}

// goplusPCHeader is the pcHeader of every registered code module. Only
// moduledataverify reads a module's pcHeader, and it never sees these;
// sharing one lets pcvalue recognise generated code with one comparison.
var goplusPCHeader pcHeader

// goplus_registerCode builds a module for the functions described by
// funcs (goplusFuncWords words each, sorted by entry) over [minpc,
// maxpc), and publishes it. It returns a handle for unregistering, or an
// error string.
//
//go:linkname goplus_registerCode goplus.runtime_registerCode
func goplus_registerCode(minpc, maxpc uintptr, funcs []uint32, pctab, funcnametab, filetab []byte, cutab []uint32, gofunc []byte) (handle unsafe.Pointer, err string) {
	nfunc := len(funcs) / goplusFuncWords
	if nfunc == 0 || len(funcs)%goplusFuncWords != 0 || minpc >= maxpc || maxpc-minpc > 1<<31 {
		return nil, "malformed registration"
	}

	// The per-function records: _func followed by npcdata and
	// nfuncdata uint32 offsets, each record 4-byte aligned.
	const npcdata = abi.PCDATA_StackMapIndex + 1
	const nfuncdata = abi.FUNCDATA_LocalsPointerMaps + 1
	funcSize := alignUp(unsafe.Sizeof(_func{})+(npcdata+nfuncdata)*4, 4)

	// One allocation holds the header and every table.
	off := alignUp(unsafe.Sizeof(goplusCodeHeader{}), 8)
	place := func(n, align uintptr) uintptr {
		off = alignUp(off, align)
		p := off
		off += n
		return p
	}
	pclnOff := place(uintptr(nfunc)*funcSize, 4)
	ftabOff := place(uintptr(nfunc+1)*unsafe.Sizeof(functab{}), 4)
	nbucket := (maxpc-minpc)/abi.FuncTabBucketSize + 1
	ffbOff := place(nbucket*unsafe.Sizeof(findfuncbucket{}), 4)
	pctabOff := place(uintptr(len(pctab)), 1)
	nameOff := place(uintptr(len(funcnametab)), 1)
	fileOff := place(uintptr(len(filetab)), 1)
	cuOff := place(uintptr(len(cutab))*4, 4)
	gofuncOff := place(uintptr(len(gofunc)), 8)
	size := alignUp(off, physPageSize)

	base := sysAlloc(size, &memstats.other_sys, "goplus code tables")
	if base == nil {
		return nil, "out of memory"
	}
	at := func(o uintptr) unsafe.Pointer { return add(base, o) }
	copyTo := func(o uintptr, b []byte) {
		if len(b) > 0 {
			memmove(at(o), unsafe.Pointer(&b[0]), uintptr(len(b)))
		}
	}
	copyTo(pctabOff, pctab)
	copyTo(nameOff, funcnametab)
	copyTo(fileOff, filetab)
	if len(cutab) > 0 {
		memmove(at(cuOff), unsafe.Pointer(&cutab[0]), uintptr(len(cutab))*4)
	}
	copyTo(gofuncOff, gofunc)

	h := (*goplusCodeHeader)(base)
	h.size = size
	md := &h.md
	md.pcHeader = &goplusPCHeader
	md.pctab = unsafe.Slice((*byte)(at(pctabOff)), len(pctab))
	md.funcnametab = unsafe.Slice((*byte)(at(nameOff)), len(funcnametab))
	md.filetab = unsafe.Slice((*byte)(at(fileOff)), len(filetab))
	md.cutab = unsafe.Slice((*uint32)(at(cuOff)), len(cutab))
	md.pclntable = unsafe.Slice((*byte)(at(pclnOff)), uintptr(nfunc)*funcSize)
	md.ftab = unsafe.Slice((*functab)(at(ftabOff)), nfunc+1)
	md.findfunctab = uintptr(at(ffbOff))
	md.gofunc = uintptr(at(gofuncOff))
	md.minpc, md.maxpc = minpc, maxpc
	md.text, md.etext = minpc, maxpc
	md.modulename = "goplus generated code"

	for i := 0; i < nfunc; i++ {
		w := funcs[i*goplusFuncWords:]
		fo := uintptr(i) * funcSize
		f := (*_func)(unsafe.Pointer(&md.pclntable[fo]))
		f.entryOff = w[goplusFuncEntryOff]
		f.nameOff = int32(w[goplusFuncNameOff])
		f.args = 0
		f.pcsp = w[goplusFuncPCSP]
		f.pcfile = w[goplusFuncPCFile]
		f.pcln = w[goplusFuncPCLn]
		f.npcdata = npcdata
		f.cuOffset = 0
		f.startLine = int32(w[goplusFuncStartLine])
		f.funcID = abi.FuncIDNormal
		f.nfuncdata = nfuncdata
		pcdata := (*[npcdata]uint32)(add(unsafe.Pointer(&f.nfuncdata), unsafe.Sizeof(f.nfuncdata)))
		pcdata[abi.PCDATA_UnsafePoint] = w[goplusFuncPCUnsafePoint]
		pcdata[abi.PCDATA_StackMapIndex] = w[goplusFuncPCStackMap]
		funcdata := (*[nfuncdata]uint32)(add(unsafe.Pointer(pcdata), npcdata*4))
		funcdata[abi.FUNCDATA_ArgsPointerMaps] = ^uint32(0)
		funcdata[abi.FUNCDATA_LocalsPointerMaps] = w[goplusFuncLocalsMap]
		md.ftab[i] = functab{entryoff: f.entryOff, funcoff: uint32(fo)}
		if i > 0 && md.ftab[i].entryoff <= md.ftab[i-1].entryoff {
			sysFree(base, size, &memstats.other_sys)
			return nil, "functions not sorted"
		}
	}
	md.ftab[nfunc] = functab{entryoff: uint32(maxpc - minpc), funcoff: md.ftab[nfunc-1].funcoff}

	// findfunctab: for every 256-byte subbucket, the index of the last
	// function starting at or before it, relative to its bucket's first.
	const nsub = uintptr(len(findfuncbucket{}.subbuckets))
	buckets := unsafe.Slice((*findfuncbucket)(at(ffbOff)), nbucket)
	idx := 0
	for b := uintptr(0); b < nbucket; b++ {
		for s := uintptr(0); s < nsub; s++ {
			pcOff := uint32(b*abi.FuncTabBucketSize + s*(abi.FuncTabBucketSize/nsub))
			for idx+1 < nfunc && md.ftab[idx+1].entryoff <= pcOff {
				idx++
			}
			if s == 0 {
				buckets[b].idx = uint32(idx)
			}
			d := idx - int(buckets[b].idx)
			if d > 255 {
				sysFree(base, size, &memstats.other_sys)
				return nil, "more than 255 functions start within one 4096-byte span"
			}
			buckets[b].subbuckets[s] = byte(d)
		}
	}

	// Refuse overlap with compiled code or another registration.
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		if minpc < datap.maxpc && datap.minpc < maxpc {
			sysFree(base, size, &memstats.other_sys)
			return nil, "code range overlaps a loaded module"
		}
	}
	for {
		old := goplusCode.Load()
		var mods []*moduledata
		if old != nil {
			mods = old.mods
		}
		pos := 0
		for pos < len(mods) && mods[pos].minpc < minpc {
			pos++
		}
		if pos > 0 && mods[pos-1].maxpc > minpc || pos < len(mods) && mods[pos].minpc < maxpc {
			sysFree(base, size, &memstats.other_sys)
			return nil, "code range overlaps registered code"
		}
		next := make([]*moduledata, 0, len(mods)+1)
		next = append(next, mods[:pos]...)
		next = append(next, md)
		next = append(next, mods[pos:]...)
		if goplusCode.CompareAndSwap(old, &goplusCodeSet{mods: next}) {
			return base, ""
		}
	}
}

// goplus_unregisterCode removes the registration h and frees its tables,
// unless a goroutine stack still holds a frame in its code.
//
//go:linkname goplus_unregisterCode goplus.runtime_unregisterCode
func goplus_unregisterCode(h unsafe.Pointer) string {
	hdr := (*goplusCodeHeader)(h)
	md := &hdr.md
	for {
		// Build the replacement set first: nothing allocates while the
		// world is stopped.
		snap := goplusCode.Load()
		var old []*moduledata
		if snap != nil {
			old = snap.mods
		}
		next := make([]*moduledata, 0, len(old))
		for _, m := range old {
			if m != md {
				next = append(next, m)
			}
		}
		if len(next) == len(old) {
			return "not registered"
		}
		set := &goplusCodeSet{mods: next}

		stw := stopTheWorld(stwGoplusCode)
		if goplusCode.Load() != snap {
			// A registration was published meanwhile; rebuild.
			startTheWorld(stw)
			continue
		}
		inUse := false
		systemstack(func() {
			// Every goroutine is stopped, including this one, whose
			// resume point systemstack saved in its sched, so each
			// stack can be unwound from its saved state.
			var u unwinder
			forEachG(func(gp *g) {
				if inUse || readgstatus(gp) == _Gdead {
					return
				}
				for u.init(gp, unwindSilentErrors); u.valid(); u.next() {
					if md.minpc <= u.frame.pc && u.frame.pc < md.maxpc {
						inUse = true
						return
					}
				}
			})
		})
		if inUse {
			startTheWorld(stw)
			return "code is on a goroutine stack"
		}
		goplusCode.Store(set)
		startTheWorld(stw)

		// No goroutine has a frame in the code, and the module was
		// unpublished with the world stopped, so no lookup or unwind
		// that could reach it is still in progress. pcvalue never
		// caches results for generated code, so new code registered
		// at these addresses cannot be described by stale entries.
		sysFree(h, hdr.size, &memstats.other_sys)
		return ""
	}
}
