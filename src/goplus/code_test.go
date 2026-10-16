// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"goplus/internal/execmem"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"
	"weak"
)

// frameFunc is frameCode's Go type.
type frameFunc = func(obj unsafe.Pointer, callee uintptr) uintptr

// What jitCallee observed, for the test to check.
var callee struct {
	weak       weak.Pointer[wbNode]
	alive      bool   // the object survived collections during the call
	stackMoved bool   // the goroutine stack was copied during the call
	trace      string // runtime.Stack from inside the call
	block      chan struct{}
	entered    chan struct{}
}

// jitCallee is called from generated code. By then the generated frame
// holds the only reference to the object callee.weak points to.
//
//go:noinline
func jitCallee() {
	runtime.GC()
	runtime.GC()
	callee.alive = callee.weak.Value() != nil
	var local byte
	before := uintptr(unsafe.Pointer(&local))
	growStack(64)
	callee.stackMoved = uintptr(unsafe.Pointer(&local)) != before
	buf := make([]byte, 1<<16)
	callee.trace = string(buf[:runtime.Stack(buf, false)])
	if callee.block != nil {
		close(callee.entered)
		<-callee.block
	}
}

//go:noinline
func growStack(n int) byte {
	var pad [512]byte
	pad[n%len(pad)] = byte(n)
	if n == 0 {
		return pad[0]
	}
	return growStack(n-1) + pad[n%len(pad)]
}

//go:noinline
func newWBNode() unsafe.Pointer { return newNode(42) }

// registerFrameCode maps frameCode and registers it, declaring as
// pointers the slots in pointers.
func registerFrameCode(t *testing.T, pointers []uint32) (frameFunc, *goplus.CodeRange, *execmem.Code) {
	t.Helper()
	code, frame, safe := frameCode()
	c := mapCode(t, code)
	r, err := goplus.RegisterCode([]goplus.Func{{
		Name:       "goplus_test.generatedFrame",
		Entry:      c.Addr(),
		End:        c.Addr() + uintptr(len(code)),
		Frame:      frame,
		SafePoints: []goplus.SafePoint{{Off: safe, Pointers: pointers}},
		File:       "generated.jit",
		Lines:      []goplus.LineStep{{Off: 0, Line: 7}},
	}})
	if err != nil {
		t.Fatalf("RegisterCode: %v", err)
	}
	return execmem.Func[frameFunc](c), r, c
}

// runFrame calls fn from a fresh goroutine, whose small stack the callee
// is sure to outgrow, and returns fn's result.
func runFrame(fn frameFunc) uintptr {
	done := make(chan uintptr)
	go func() {
		obj := newWBNode()
		callee.weak = weak.Make((*wbNode)(obj))
		done <- fn(obj, reflect.ValueOf(jitCallee).Pointer())
	}()
	return <-done
}

// TestRegisterCodeFrame runs a goroutine whose stack holds a registered
// generated frame through a collection, a stack copy and a traceback.
func TestRegisterCodeFrame(t *testing.T) {
	fn, r, _ := registerFrameCode(t, framePointerSlots)
	defer r.Unregister()

	delta := runFrame(fn)
	if !callee.stackMoved {
		t.Fatal("the callee's stack growth did not copy the stack; the test proves nothing about stack copying")
	}
	if !callee.alive {
		t.Error("an object referenced only from a declared pointer slot of the generated frame was collected")
	}
	if delta != 0 {
		t.Errorf("a declared pointer slot holding a stack address was not adjusted by the stack copy (off by %d)", int(delta))
	}
	for _, want := range []string{"goplus_test.generatedFrame", "generated.jit:7", "goplus_test.jitCallee"} {
		if !strings.Contains(callee.trace, want) {
			t.Errorf("traceback from inside the call lacks %q:\n%s", want, callee.trace)
		}
	}
}

// TestRegisterCodeFrameUndeclared is TestRegisterCodeFrame's control:
// the same code registered without declaring its pointer slots. The
// object must then be collected and the stack slot left unadjusted, which
// shows the declared slots are what made the other test pass.
func TestRegisterCodeFrameUndeclared(t *testing.T) {
	fn, r, _ := registerFrameCode(t, nil)
	defer r.Unregister()

	delta := runFrame(fn)
	if !callee.stackMoved {
		t.Fatal("the callee's stack growth did not copy the stack")
	}
	if callee.alive {
		t.Error("an object referenced only from an undeclared frame slot survived two collections")
	}
	if delta == 0 {
		t.Error("an undeclared frame slot was adjusted by the stack copy")
	}
}

// TestUnregisterCodeOnStack checks that code cannot be unregistered while
// a goroutine stack holds a frame of it, and can be once it returns.
func TestUnregisterCodeOnStack(t *testing.T) {
	fn, r, c := registerFrameCode(t, framePointerSlots)
	callee.block = make(chan struct{})
	callee.entered = make(chan struct{})
	defer func() { callee.block = nil }()

	done := make(chan uintptr)
	go func() { done <- fn(newWBNode(), reflect.ValueOf(jitCallee).Pointer()) }()
	<-callee.entered
	if err := r.Unregister(); err == nil || !strings.Contains(err.Error(), "on a goroutine stack") {
		t.Fatalf("Unregister with a parked goroutine in the code: err = %v, want an on-stack error", err)
	}
	close(callee.block)
	<-done
	if err := r.Unregister(); err != nil {
		t.Fatalf("Unregister after the goroutine returned: %v", err)
	}
	if f := runtime.FuncForPC(c.Addr()); f != nil {
		t.Errorf("FuncForPC still finds %s after Unregister", f.Name())
	}
	if err := r.Unregister(); err == nil {
		t.Error("second Unregister succeeded")
	}

	// The same addresses can be registered again.
	code, frame, safe := frameCode()
	r2, err := goplus.RegisterCode([]goplus.Func{{
		Name: "again", Entry: c.Addr(), End: c.Addr() + uintptr(len(code)), Frame: frame,
		SafePoints: []goplus.SafePoint{{Off: safe}},
	}})
	if err != nil {
		t.Fatalf("re-registering after Unregister: %v", err)
	}
	if f := runtime.FuncForPC(c.Addr()); f == nil || f.Name() != "again" {
		t.Errorf("FuncForPC after re-registering = %v", f)
	}
	r2.Unregister()
}

func TestRegisterCodeRejects(t *testing.T) {
	code, frame, safe := frameCode()
	if code == nil {
		t.Skip("no generated code on this architecture")
	}
	base := uintptr(0x10000000) // never executed; registration only
	end := base + uintptr(len(code))
	ok := goplus.Func{Name: "f", Entry: base, End: end, Frame: frame, SafePoints: []goplus.SafePoint{{Off: safe}}}
	goPC := reflect.ValueOf(TestRegisterCodeRejects).Pointer()

	for _, tc := range []struct {
		name  string
		funcs func() []goplus.Func
		want  string
	}{
		{"none", func() []goplus.Func { return nil }, "no functions"},
		{"empty range", func() []goplus.Func { f := ok; f.End = f.Entry; return []goplus.Func{f} }, "range"},
		{"frame not at 0", func() []goplus.Func { f := ok; f.Frame = frame[1:]; return []goplus.Func{f} }, "offset 0"},
		{"pointer above locals", func() []goplus.Func {
			f := ok
			f.SafePoints = []goplus.SafePoint{{Off: safe, Pointers: []uint32{frame[1].Size}}}
			return []goplus.Func{f}
		}, "outside the frame"},
		{"overlapping funcs", func() []goplus.Func { g := ok; g.Name = "g"; return []goplus.Func{ok, g} }, "overlaps"},
		{"compiled code", func() []goplus.Func {
			f := ok
			f.Entry, f.End = goPC, goPC+uintptr(len(code))
			return []goplus.Func{f}
		}, "overlaps a loaded module"},
	} {
		if _, err := goplus.RegisterCode(tc.funcs()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}

	// Two registrations of one range: the second is refused.
	r, err := goplus.RegisterCode([]goplus.Func{ok})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Unregister()
	if _, err := goplus.RegisterCode([]goplus.Func{ok}); err == nil || !strings.Contains(err.Error(), "overlaps registered code") {
		t.Errorf("second registration of one range: err = %v", err)
	}
}

// abiOut receives what abiProbeCode stores.
var abiOut [32]uintptr

// TestABIInternalRegisters pins the register assignment the package
// documentation states: generated code reads each argument from the
// documented register and returns results in them. It fails if a Go
// release moves an argument or result.
func TestABIInternalRegisters(t *testing.T) {
	code := abiProbeCode(uintptr(unsafe.Pointer(&abiOut)))
	c := mapCode(t, code)
	abiOut = [32]uintptr{}
	results := callABIProbe(execmem.Func[abiProbeFunc](c))
	n := len(abiIntRegs)
	for i := range n {
		if abiOut[i] != uintptr(i+1) {
			t.Errorf("integer argument %d: register %d holds %d, want %d", i, abiIntRegs[i], abiOut[i], i+1)
		}
		if results[i] != uintptr(100+i) {
			t.Errorf("integer result %d: got %d from register %d, want %d", i, results[i], abiIntRegs[i], 100+i)
		}
	}
	for x := range abiFloatRegs {
		got := *(*float64)(unsafe.Pointer(&abiOut[n+x]))
		if want := float64(x) + 0.5; got != want {
			t.Errorf("float argument %d: register holds %v, want %v", x, got, want)
		}
	}
	if runtime.GOARCH == "amd64" {
		if x15 := abiOut[n+15]; x15 != 0 {
			t.Errorf("X15 = %#x on entry from Go, want 0", x15)
		}
	}
}
