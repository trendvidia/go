// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"fmt"
	"goplus"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
	"weak"
)

const ptrSize = unsafe.Sizeof(uintptr(0))

type leaf struct{ v [8]uint64 }

// newLeaf returns a leaf holding v, and a flag the collector sets when
// it frees the leaf.
func newLeaf(v uint64) (*leaf, *atomic.Bool) {
	c := &leaf{}
	c.v[0] = v
	freed := new(atomic.Bool)
	runtime.AddCleanup(c, func(f *atomic.Bool) { f.Store(true) }, freed)
	return c, freed
}

// churn runs collections while allocating pointer-free blocks of the
// size of a layout's type descriptor, filled with a pattern, so that a
// type freed too early has its memory reused and overwritten.
func churn() {
	var junk [][]uintptr
	for range 10 {
		runtime.GC()
		for range 2000 {
			b := make([]uintptr, 10)
			for i := range b {
				b[i] = 0xdeadbeef
			}
			junk = append(junk, b)
		}
		junk = junk[len(junk)/2:]
	}
	runtime.KeepAlive(junk)
}

// headerMin is the smallest object for which the runtime records the
// type with the object (a malloc header, or a large object's span) and
// reads it back on every scan; below it the type is not needed after
// allocation. It is one pointer mask word's worth of words: 512 bytes
// on 64-bit ports, 128 on 32-bit ones.
const headerMin = ptrSize*8*ptrSize + 1

// TestLayoutKeepsTypeAlive drops a layout while an object allocated
// from it lives on, and checks that the collector still scans the
// object correctly, then that the type is freed once the object dies.
// Without the runtime keeping the type alive through the object, the
// collector reads the freed type and faults (trendvidia/go#12).
func TestLayoutKeepsTypeAlive(t *testing.T) {
	for _, size := range []uintptr{64, 1024, 64 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			l := goplus.NewLayout(size, ptrSize, []byte{1})
			obj := l.New()
			c, freed := newLeaf(42)
			*(**leaf)(obj) = c
			wt := weak.Make((*byte)(goplus.LayoutType(l)))
			l, c = nil, nil

			churn()
			if got := (*(**leaf)(obj)).v[0]; got != 42 || freed.Load() {
				t.Fatalf("leaf reachable only through the object: value %d, freed %v", got, freed.Load())
			}
			if size >= headerMin && wt.Value() == nil {
				t.Fatal("the type was freed while an object allocated from it is live")
			}
			runtime.KeepAlive(obj)

			// Once the object is gone, nothing holds the type.
			obj = nil
			for i := 0; i < 10 && wt.Value() != nil; i++ {
				runtime.GC()
			}
			if wt.Value() != nil {
				t.Fatal("the type was not freed after its last object died")
			}
		})
	}
}

// TestLayoutPointerMask checks both halves of the mask: a pointer word
// keeps what it points to alive, and an address stored in a scalar word
// does not. The collector reads a mask one word (64 or 32 bits) at a
// time, so some layouts have their pointers past the mask's first word,
// and one has none in its first word at all.
func TestLayoutPointerMask(t *testing.T) {
	for _, tt := range []struct {
		size   uintptr
		ptrs   []uintptr // pointer words, ascending
		scalar uintptr   // a scalar word
	}{
		{64, []uintptr{1, 3, 6}, 2},
		{1024, []uintptr{1, 3, 6}, 2},
		{1024, []uintptr{70, 127}, 65},
		{64 << 10, []uintptr{1, 70, 1000}, 65},
	} {
		t.Run(fmt.Sprint(tt.size, tt.ptrs), func(t *testing.T) {
			mask := make([]byte, tt.ptrs[len(tt.ptrs)-1]/8+1)
			for _, w := range tt.ptrs {
				mask[w/8] |= 1 << (w % 8)
			}
			l := goplus.NewLayout(tt.size, ptrSize, mask)
			obj := l.New()
			words := unsafe.Slice((*uintptr)(obj), tt.size/ptrSize)
			var kept []*atomic.Bool
			for _, w := range tt.ptrs {
				c, freed := newLeaf(uint64(w))
				*(**leaf)(unsafe.Pointer(&words[w])) = c
				kept = append(kept, freed)
			}
			// A leaf whose only reference is an address in a scalar word.
			c, scalarFreed := newLeaf(uint64(tt.scalar))
			words[tt.scalar] = uintptr(unsafe.Pointer(c))
			c = nil

			for i := 0; i < 10 && !scalarFreed.Load(); i++ {
				runtime.GC()
			}
			if !scalarFreed.Load() {
				t.Error("an address in a scalar word kept its target alive: the mask marks it as a pointer")
			}
			for i, w := range tt.ptrs {
				if got := (*(**leaf)(unsafe.Pointer(&words[w]))).v[0]; got != uint64(w) || kept[i].Load() {
					t.Errorf("pointer word %d: value %d, freed %v", w, got, kept[i].Load())
				}
			}
			runtime.KeepAlive(obj)
		})
	}
}

// TestLayoutNoPointers allocates from layouts without pointer words: tiny,
// small, header-sized and large, with and without a mask. Their objects
// record no type, and an address stored in one keeps nothing alive.
func TestLayoutNoPointers(t *testing.T) {
	for _, tt := range []struct {
		size, align uintptr
		mask        []byte
	}{
		{1, 1, nil},
		{12, 4, nil},
		{ptrSize, ptrSize, []byte{0}},
		{1024, ptrSize, nil},
		{64 << 10, ptrSize, make([]byte, 64)},
	} {
		t.Run(fmt.Sprint(tt.size), func(t *testing.T) {
			l := goplus.NewLayout(tt.size, tt.align, tt.mask)
			obj := l.New()
			b := unsafe.Slice((*byte)(obj), tt.size)
			for i := range b {
				if b[i] != 0 {
					t.Fatalf("byte %d of a new object is %#x, not zero", i, b[i])
				}
				b[i] = byte(i)
			}
			// An address in word 0, which the checks below skip.
			start := uintptr(0)
			var scalarFreed *atomic.Bool
			if tt.size >= ptrSize {
				var c *leaf
				c, scalarFreed = newLeaf(1)
				*(*uintptr)(obj) = uintptr(unsafe.Pointer(c))
				c = nil
				start = ptrSize
			}
			l = nil
			for i := 0; i < 10 && scalarFreed != nil && !scalarFreed.Load(); i++ {
				runtime.GC()
			}
			churn()
			for i := start; i < tt.size; i++ {
				if b[i] != byte(i) {
					t.Fatalf("byte %d is %#x, want %#x", i, b[i], byte(i))
				}
			}
			if scalarFreed != nil && !scalarFreed.Load() {
				t.Error("an address in a pointer-free object kept its target alive")
			}
			runtime.KeepAlive(obj)
		})
	}
}

func TestLayoutNewArray(t *testing.T) {
	const n = 100
	l := goplus.NewLayout(3*ptrSize, ptrSize, []byte{1 << 1}) // word 1 of 3
	if l.Size() != 3*ptrSize {
		t.Fatalf("Size() = %d", l.Size())
	}
	arr := l.NewArray(n)
	words := unsafe.Slice((*uintptr)(arr), 3*n)
	flags := make([]*atomic.Bool, n)
	for i := range n {
		c, freed := newLeaf(uint64(i))
		words[3*i] = 0xdeadbeef
		*(**leaf)(unsafe.Pointer(&words[3*i+1])) = c
		words[3*i+2] = 0xdeadbeef
		flags[i] = freed
	}
	l = nil
	churn()
	for i := range n {
		if got := (*(**leaf)(unsafe.Pointer(&words[3*i+1]))).v[0]; got != uint64(i) || flags[i].Load() {
			t.Fatalf("element %d: value %d, freed %v", i, got, flags[i].Load())
		}
	}
	runtime.KeepAlive(arr)

	l = goplus.NewLayout(ptrSize, ptrSize, nil)
	defer func() {
		if recover() == nil {
			t.Error("NewArray(-1) did not panic")
		}
	}()
	l.NewArray(-1)
}

// TestLayoutConcurrentGC allocates from short-lived layouts while
// collections run back to back, so that objects are allocated while the
// collector is marking: those are allocated black and never scanned in
// that cycle, so the allocation itself must keep the type alive.
func TestLayoutConcurrentGC(t *testing.T) {
	var cycles atomic.Int64
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
				cycles.Add(1)
			}
		}
	}()

	type held struct {
		obj   unsafe.Pointer
		freed *atomic.Bool
		v     uint64
	}
	var objs []held
	// Bound by collections, not iterations: the point is to overlap many.
	for i := uint64(0); cycles.Load() < 300; i++ {
		size := uintptr(1024)
		if i%8 == 0 {
			size = 40 << 10 // large objects take the span's type path
		}
		l := goplus.NewLayout(size, ptrSize, []byte{1})
		obj := l.New()
		c, freed := newLeaf(i)
		*(**leaf)(obj) = c
		objs = append(objs, held{obj, freed, i})
		if len(objs) > 2000 {
			objs = objs[1000:]
		}
	}
	close(stop)
	wg.Wait()
	churn()
	for _, h := range objs {
		if got := (*(**leaf)(h.obj)).v[0]; got != h.v || h.freed.Load() {
			t.Fatalf("object %d: value %d, freed %v", h.v, got, h.freed.Load())
		}
	}
	t.Logf("%d collections, %d objects checked", cycles.Load(), len(objs))
}

// TestLayoutLargeAllocDuringMark aims at one window. A layout made
// before a collection starts is held only on a goroutine's stack; after
// marking starts, and before the collector scans that stack, the
// goroutine allocates a large object from it (so black: never scanned
// this cycle) and drops the layout. The type is then reachable only
// through the object's span, which is written without a write barrier,
// so the allocation itself must keep the type alive.
//
// Each worker builds a batch of layouts first, in a stack-local array,
// and then consumes the batch in a burst, so that a burst spanning the
// start of marking opens the window for many layouts at once; several
// workers each have their stack scanned at a different moment.
func TestLayoutLargeAllocDuringMark(t *testing.T) {
	var cycles atomic.Int64
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
				cycles.Add(1)
			}
		}
	}()
	const workers, batch = 8, 64
	type held struct {
		obj   unsafe.Pointer
		freed *atomic.Bool
		v     uint64
	}
	results := make([][]held, workers)
	var ww sync.WaitGroup
	for w := range workers {
		ww.Add(1)
		go func() {
			defer ww.Done()
			var objs []held
			for v := uint64(0); cycles.Load() < 200; {
				var ls [batch]*goplus.Layout // on the stack only
				for i := range ls {
					ls[i] = goplus.NewLayout(32<<10+8, ptrSize, []byte{1})
				}
				for i := range ls {
					obj := ls[i].New()
					ls[i] = nil
					c, freed := newLeaf(v)
					*(**leaf)(obj) = c
					objs = append(objs, held{obj, freed, v})
					v++
				}
				if len(objs) > 4*batch {
					objs = objs[2*batch:]
				}
			}
			results[w] = objs
		}()
	}
	ww.Wait()
	close(stop)
	wg.Wait()
	churn()
	n := 0
	for _, objs := range results {
		for _, h := range objs {
			if got := (*(**leaf)(h.obj)).v[0]; got != h.v || h.freed.Load() {
				t.Fatalf("object %d: value %d, freed %v", h.v, got, h.freed.Load())
			}
			n++
		}
	}
	t.Logf("%d collections, %d objects checked", cycles.Load(), n)
}

// TestLayoutHeapReturnsToBaseline is #12's done-when: creating and
// dropping 100,000 layouts, with an object allocated from each, returns
// the heap to where it started.
func TestLayoutHeapReturnsToBaseline(t *testing.T) {
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	base := heap()
	objs := make([]unsafe.Pointer, 100_000)
	for i := range objs {
		// 576 bytes: above the size that records its type per object.
		objs[i] = goplus.NewLayout(576, ptrSize, []byte{1}).New()
	}
	held := heap()
	runtime.KeepAlive(objs)
	objs = nil
	after := heap()
	t.Logf("heap: base %d KB, holding %d KB, after %d KB", base>>10, held>>10, after>>10)
	if held < base+50<<20 {
		t.Fatalf("holding 100,000 objects grew the heap by only %d KB: the test measured nothing", (held-base)>>10)
	}
	if after > base+2<<20 {
		t.Fatalf("heap did not return to its baseline: %d KB more than before", (after-base)>>10)
	}
}

// TestLayoutHeapDump writes a heap dump while layout objects are live.
// The dump names each object's type, and a type in the heap is in no
// module, so its name resolves through the runtime's registry; without
// the name NewLayout registers, the dump throws.
func TestLayoutHeapDump(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "heapdump")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	objs := []unsafe.Pointer{
		goplus.NewLayout(1024, ptrSize, []byte{1}).New(),
		goplus.NewLayout(64<<10, ptrSize, []byte{1}).New(),
	}
	debug.WriteHeapDump(f.Fd())
	runtime.KeepAlive(objs)
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("empty heap dump")
	}
}

func TestNewLayoutPanics(t *testing.T) {
	type panicCase struct {
		name        string
		size, align uintptr
		mask        []byte
	}
	tests := []panicCase{
		{"zero size", 0, ptrSize, nil},
		{"zero align", ptrSize, 0, nil},
		{"align not a power of two", 12, 3, nil},
		{"align above pointer size", 32, 2 * ptrSize, nil},
		{"size not a multiple of align", 6, 4, nil},
		{"pointers without pointer alignment", 4 * ptrSize, ptrSize / 2, []byte{1}},
	}
	if ptrSize == 8 {
		// Larger than any object the runtime can allocate. A 32-bit port
		// can allocate up to the top of its address space, so there is no
		// such size there.
		tests = append(tests, panicCase{"size above the allocation limit", 1 << (8*ptrSize - 8), ptrSize, nil})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewLayout(%d, %d, %v) did not panic", tt.size, tt.align, tt.mask)
				}
			}()
			goplus.NewLayout(tt.size, tt.align, tt.mask)
		})
	}
	// Valid edge cases.
	goplus.NewLayout(1, 1, nil)
	goplus.NewLayout(4, 4, []byte{0})                // a mask with no pointers needs no alignment
	goplus.NewLayout(ptrSize, ptrSize, []byte{0xff}) // bits past the end are ignored
}

var structOfSeq int

// BenchmarkNewLayout compares the cost of a new layout with
// reflect.StructOf for a struct of the same shape, each iteration a
// layout StructOf has not seen (its cache would make repeats cheap).
func BenchmarkNewLayout(b *testing.B) {
	b.Run("goplus.NewLayout", func(b *testing.B) {
		for i := range b.N {
			goplus.NewLayout(uintptr(16+8*(i%1024)), 8, []byte{1})
		}
	})
	b.Run("reflect.StructOf", func(b *testing.B) {
		ptr := reflect.TypeOf((*int)(nil))
		u8 := reflect.TypeOf(byte(0))
		for range b.N {
			// Distinct per iteration, across runs too: StructOf caches
			// by layout, and a repeat would measure the cache.
			structOfSeq++
			reflect.StructOf([]reflect.StructField{
				{Name: "P", Type: ptr},
				{Name: "Pad", Type: reflect.ArrayOf(8+structOfSeq, u8)},
			})
		}
	})
}
