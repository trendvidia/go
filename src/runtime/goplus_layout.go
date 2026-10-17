// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The runtime half of goplus.NewLayout (a go-plus addition).

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// goplus_newLayoutType returns a type for objects of the given size and
// alignment whose pointer words are the set bits of mask (bit i of byte
// i/8 for word i), the last of them word ptrWords-1; ptrWords is 0 if
// there are none. The caller (goplus.NewLayout) has validated them, all
// but the size's upper bound: it returns nil for a size larger than any
// object the runtime can allocate.
//
// The type and its pointer mask are ordinary heap objects. Nothing
// caches them: they are collected when no object allocated from the
// type, and nothing else, refers to the type. Objects keep it alive
// through shadeObjectType.
//
// It is a struct type with no fields, which is what the runtime needs
// of it: a size, an alignment and a pointer mask. It must not be given
// to reflect, whose caches would keep it alive forever, nor used as a
// channel or map element type: an hchan records its element type where
// the collector does not trace it.
//
//go:linkname goplus_newLayoutType goplus.runtime_newLayoutType
func goplus_newLayoutType(size, align, ptrWords uintptr, mask []byte) unsafe.Pointer {
	if size > maxAlloc {
		return nil
	}
	st := new(abi.StructType)
	t := &st.Type
	t.Size_ = size
	t.Align_ = uint8(align)
	t.FieldAlign_ = uint8(align)
	t.Kind_ = abi.Struct
	t.Hash = cheaprand()
	t.Str = layoutTypeName()

	if ptrWords != 0 {
		t.PtrBytes = ptrWords * goarch.PtrSize
		// The collector reads a mask a pointer-sized word at a time from
		// an aligned address (readUintptr), so it is stored in whole,
		// aligned words, its bytes in mask order.
		n := (ptrWords + 7) / 8
		maskWords := make([]uintptr, (n+goarch.PtrSize-1)/goarch.PtrSize)
		gcdata := unsafe.Slice((*byte)(unsafe.Pointer(&maskWords[0])), n)
		copy(gcdata, mask)
		// Clear any bits past the last pointer word.
		if r := ptrWords % 8; r != 0 {
			gcdata[n-1] &= 1<<r - 1
		}
		t.GCData = &gcdata[0]
	}
	return unsafe.Pointer(t)
}

// goplus_layoutNew allocates one zeroed object of the layout type t.
//
//go:linkname goplus_layoutNew goplus.runtime_layoutNew
func goplus_layoutNew(t unsafe.Pointer) unsafe.Pointer {
	return newobject((*_type)(t))
}

// goplus_layoutNewArray allocates n contiguous zeroed objects of the
// layout type t.
//
//go:linkname goplus_layoutNewArray goplus.runtime_layoutNewArray
func goplus_layoutNewArray(t unsafe.Pointer, n int) unsafe.Pointer {
	return newarray((*_type)(t), n)
}

// layoutNameOff is the name every layout type reports, registered once:
// a type allocated in the heap is in no module, so its Str is resolved
// through reflectOffs (see resolveNameOff). Code that prints a type, a
// heap dump or an execution trace with allocation events among them,
// would otherwise throw on a layout type.
var layoutNameOff atomic.Int32

func layoutTypeName() abi.NameOff {
	if off := layoutNameOff.Load(); off != 0 {
		return abi.NameOff(off)
	}
	// reflect_addReflectOff returns the same id for the same pointer, so
	// a race here registers one name or two, never one per type.
	n := abi.NewName("goplus.Layout", "", false, false)
	off := reflect_addReflectOff(unsafe.Pointer(n.Bytes))
	layoutNameOff.CompareAndSwap(0, off)
	return abi.NameOff(layoutNameOff.Load())
}

// isLayoutType reports whether typ was made by goplus_newLayoutType: such
// a type, unlike any other, can be freed. Every layout type, and no
// other, reports layoutNameOff as its name. typ must be live.
func isLayoutType(typ *abi.Type) bool {
	off := layoutNameOff.Load()
	return off != 0 && typ.Str == abi.NameOff(off)
}
