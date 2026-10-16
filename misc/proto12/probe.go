//go:build ignore

// Probe for trendvidia/go#12: is a type kept alive by the objects
// allocated from it? Builds an uncached copy of a StructOf type in
// noscan memory, allocates one object from it, drops every reference to
// the type, reuses its memory, and watches whether the collector still
// scans the object correctly (a child reachable only through it must
// survive).
package main

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"sync/atomic"
	"unsafe"
	_ "unsafe" // linkname
	"weak"
)

type child struct{ v [64]byte }

// unsafe_New allocates without reflect.New's ptrTo, whose cache would
// keep the type alive.
//
//go:linkname unsafe_New reflect.unsafe_New
func unsafe_New(t unsafe.Pointer) unsafe.Pointer

var finalized atomic.Bool

func main() {
	size, _ := strconv.Atoi(os.Args[1]) // object size in bytes
	fields := []reflect.StructField{{Name: "P", Type: reflect.TypeOf((*child)(nil))}}
	if size > 8 {
		fields = append(fields, reflect.StructField{Name: "Pad", Type: reflect.ArrayOf(size-8, reflect.TypeOf(byte(0)))})
	}
	base := reflect.StructOf(fields) // cached by reflect forever; its GC data stays valid

	// Copy base's type descriptor into noscan memory the collector may free.
	const descBytes = 256
	cp := make([]uintptr, descBytes/8)
	src := (*[2]unsafe.Pointer)(unsafe.Pointer(&base))[1]
	copy(cp, unsafe.Slice((*uintptr)(src), descBytes/8))
	var t reflect.Type = base
	(*[2]unsafe.Pointer)(unsafe.Pointer(&t))[1] = unsafe.Pointer(&cp[0])
	if t.Size() != base.Size() {
		panic("copy is not a type")
	}

	p := unsafe_New(unsafe.Pointer(&cp[0])) // the runtime records the copy for this object
	c := &child{}
	c.v[0] = 42
	runtime.SetFinalizer(c, func(*child) { finalized.Store(true) })
	*(**child)(p) = c
	descAddr := uintptr(unsafe.Pointer(&cp[0]))
	wtype := weak.Make(&cp[0])
	c, t, cp = nil, nil, nil

	// Free the copy and overwrite its memory with a recognisable pattern.
	var junk [][]uintptr
	for round := 0; round < 20; round++ {
		runtime.GC()
		for range 2000 {
			b := make([]uintptr, descBytes/8)
			for i := range b {
				b[i] = 0x00000000deadbeef
			}
			junk = append(junk, b)
		}
		junk = junk[len(junk)/2:]
	}
	reused := false
	for _, b := range junk {
		if uintptr(unsafe.Pointer(&b[0])) == descAddr {
			reused = true
		}
	}
	runtime.GC()
	runtime.GC()
	alive := (*(**child)(p)).v[0] == 42
	fmt.Printf("size=%d type-memory-reused=%v finalized-while-reachable=%v child-intact=%v\n", size, reused, finalized.Load(), alive)
	runtime.KeepAlive(p)

	// Second half: once the object is unreachable, its type must go too.
	p = nil
	for range 4 {
		runtime.GC()
	}
	fmt.Printf("size=%d type-freed-after-object=%v\n", size, wtype.Value() == nil)
}
