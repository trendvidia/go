// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"goplus"
	"iter"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// TestGoplusRuntimeOwned pins which callers a goplus.AllocDomain stack
// limit never refuses at, by the symbol-name shapes the toolchain emits.
func TestGoplusRuntimeOwned(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"runtime.deferreturn", true},
		{"runtime.gopanic", true},
		{"runtime.mapassign_faststr", true},
		{"internal/runtime/maps.(*Map).Clear", true},
		{"internal/runtime/maps.(*Map).putSlotSmall", true},
		{"sync.(*Mutex).Lock", true},
		{"internal/sync.(*Mutex).lockSlow", true},
		{"sync.OnceValue[go.shape.int].func1", true},
		{"type:.hash.main.key", true},
		{"type:.eq.[2]interface {}", true},
		// Implemented in the runtime, named for the package it serves.
		{"sync.runtime_Semacquire", true},
		{"internal/poll.runtime_pollWait", true},
		{"os.runtime_args", true},
		{"example.com/x.runtime_F", true},

		// Helpers compiled code calls for one operation are runtime code
		// too: a refusal waits for them to return (TestGoplusLateRefusable).
		{"runtime.checkptrAlignment", true},
		{"runtime.checkptrBase", true},
		{"runtime.assertE2I2", true},
		{"runtime.getitab", true},
		{"runtime.convT", true},
		{"runtime.mapaccess1_faststr", true},
		{"runtime.growslice", true},

		{"fmt.(*pp).printValue", false},
		{"errors.is", false},
		{"main.main", false},
		{"main.runtime", false},
		{"main.(*T).runtime_x", false},
		{"runtimex.f", false},
		{"syncx.(*Mutex).Lock", false},
		{"example.com/runtime.F", false},
		{"example.com/sync.F", false},
	} {
		if got := runtime.GoplusRuntimeOwned(tc.name); got != tc.want {
			t.Errorf("goplusRuntimeOwned(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGoplusPanicSafeAt checks the classification from a return address,
// as newstack sees it: one inside a runtime or sync function, inside an
// ordinary function, and outside any function the runtime knows.
func TestGoplusPanicSafeAt(t *testing.T) {
	// A return address points just past a call, inside its function.
	inside := func(f any) uintptr { return reflect.ValueOf(f).Pointer() + 1 }
	for _, tc := range []struct {
		what string
		pc   uintptr
		want bool
	}{
		{"runtime.GC", inside(runtime.GC), false},
		{"sync.(*Mutex).Lock", inside((*sync.Mutex).Lock), false},
		{"test function", inside(TestGoplusPanicSafeAt), true},
		{"unknown code", 1, false},
	} {
		if got := runtime.GoplusPanicSafeAt(tc.pc); got != tc.want {
			t.Errorf("goplusPanicSafeAt(%s) = %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestGoplusLateRefusable pins which runtime functions a stack-limit
// refusal may wait for, to raise the panic as they return: those compiled
// code calls for an operation of the language, and not the runtime's API.
func TestGoplusLateRefusable(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"runtime.assertE2I2", true},
		{"runtime.getitab", true},
		{"runtime.checkptrArithmetic", true},
		{"runtime.mapassign_faststr", true},
		{"runtime.chanrecv1", true},
		{"runtime.deferproc", true},
		{"runtime.deferreturn", true},
		{"runtime.growslice", true},
		{"runtime._LostExternalCode", true},

		// API: callers may pair it with a release in a deferred call.
		{"runtime.LockOSThread", false},
		{"runtime.(*Pinner).Pin", false},
		{"runtime.GC", false},
		{"sync.runtime_SemacquireMutex", false},
		{"goplus.runtime_setAllocDomain", false},
		// Called by name by iter.Pull (//go:linknamestd), which needs
		// the coroutine it returns.
		{"runtime.newcoro", false},
		{"runtime.coroswitch", false},
		{"internal/poll.runtime_pollOpen", false},
		// Closures and methods are not called by compiled code directly.
		{"runtime.newproc.func1", false},
		{"runtime.(*mheap).alloc", false},
		{"internal/runtime/maps.(*Map).putSlotSmall", false},
		{"type:.eq.main.T", false},
		{"runtimex.f", false},
		{"runtime.", false},
		{"", false},
	} {
		if got := runtime.GoplusLateRefusable(tc.name); got != tc.want {
			t.Errorf("goplusLateRefusable(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// lateHelper stands for a runtime function that compiled code calls: the
// tests below have the stack limit treat it so, which makes every growth
// of their recursion come in lateLeaf, called from the runtime, on every
// platform, whatever its frame sizes. Before trendvidia/go#97 such a
// growth was let through at every level, and the recursion ran on to the
// process-wide maximum.
//
//go:noinline
func lateHelper(n int) int {
	if lateHold {
		runtime.Acquirem()
	}
	r := lateLeaf(n)
	if lateHold {
		runtime.Releasem()
	}
	lateReturns++
	if lateOwnPanic && runtime.GoplusLateRefusalPending() {
		panic("own")
	}
	return r
}

const lateHelperName = "runtime_test.lateHelper"

// lateLeaf needs more stack than lateRecurse uses per level, so the
// recursion's growths come here, whose caller is lateHelper.
//
//go:noinline
func lateLeaf(n int) int {
	var buf [16 << 10]byte
	buf[n%len(buf)] = byte(n)
	if lateCheck != nil && runtime.GoplusLateRefusalPending() {
		lateCheck()
		lateCheck = nil
	}
	return int(buf[(n+1)%len(buf)])
}

// lateFrame is about the stack each level of lateRecurse uses.
const lateFrame = 1 << 10

// lateRecurse recurses forever, calling lateHelper at each level.
//
//go:noinline
func lateRecurse(n int) int {
	var buf [lateFrame - 64]byte
	buf[n%len(buf)] = byte(n)
	lateLevels = n
	return lateHelper(n) + lateRecurse(n+1) + int(buf[(n+1)%len(buf)])
}

var (
	lateLevels, lateReturns int
	lateOwnPanic            bool
	lateHold                bool   // lateHelper holds the M's lock count up while lateLeaf runs
	lateCheck               func() // run once in lateLeaf while a refusal is pending
)

// runLate runs lateRecurse inside Run on a domain with limit and returns
// the value Run panicked with, the stack where the panic was raised, and
// how many refusals waited for a runtime call meanwhile.
func runLate(t *testing.T, limit uint64) (p any, stack []string, late uint64) {
	return runLateWith(t, limit, lateHelperName, func() { lateRecurse(0) })
}

func runLateWith(t *testing.T, limit uint64, helper string, recurse func()) (p any, stack []string, late uint64) {
	// A growth let through doubles the stack, and a refusal grows it once
	// more for the panic: past that, the process ends.
	old := debug.SetMaxStack(int(4 * limit))
	t.Cleanup(func() { debug.SetMaxStack(old) })
	runtime.SetGoplusTestRuntimeFunc(helper)
	defer runtime.SetGoplusTestRuntimeFunc("")
	lateLevels, lateReturns = 0, 0

	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)
	before := runtime.GoplusLateRefusals()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			late = runtime.GoplusLateRefusals() - before
			if runtime.GoplusLateRefusalPending() {
				t.Errorf("a refusal is still pending after the recover")
			}
		}()
		d.Run(func() {
			defer func() {
				p = recover()
				stack = callerNames(0)
			}()
			recurse()
		})
	}()
	<-done
	return p, stack, late
}

// callerNames returns the functions on the calling goroutine's stack,
// innermost first, after skip.
func callerNames(skip int) []string {
	pcs := make([]uintptr, 1<<16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(skip+2, pcs)])
	var names []string
	for {
		f, more := frames.Next()
		names = append(names, f.Function)
		if !more {
			return names
		}
	}
}

// fpCallerNames is callerNames by frame pointers, as the execution tracer
// and the block and mutex profiles walk the stack.
func fpCallerNames() []string {
	pcs := make([]uintptr, 1<<16)
	var names []string
	for _, pc := range pcs[:runtime.FPCallers(pcs)] {
		if f := runtime.FuncForPC(pc - 1); f != nil {
			names = append(names, f.Name())
		} else {
			names = append(names, "?")
		}
	}
	return names
}

// below returns the function in stack right below (called by) name: the
// caller of name, as stack lists callees first.
func below(stack []string, name string) string {
	for i, f := range stack {
		if f == name && i+1 < len(stack) {
			return stack[i+1]
		}
	}
	return ""
}

func needMaxStack(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("SetMaxStack is not enforced on %s", runtime.GOARCH)
	}
}

// TestGoplusMaxStackLate checks that a growth past the limit in a function
// the runtime called is refused once the runtime returns: the runtime
// function completes, and the panic is raised at the call to it.
func TestGoplusMaxStackLate(t *testing.T) {
	needMaxStack(t)
	const limit = 1 << 20
	p, stack, late := runLate(t, limit)
	if err, ok := p.(error); !ok || !errors.Is(err, goplus.ErrMaxStack) {
		t.Fatalf("Run panicked with %v (%T), want goplus.ErrMaxStack", p, p)
	}
	if late == 0 {
		t.Fatalf("no refusal waited for a runtime call; the test does not test that")
	}
	// Raised as lateHelper returned, in lateRecurse at the call to it.
	if got := below(stack, "runtime.goplusPanicMaxStack"); got != "runtime_test.lateRecurse" {
		t.Errorf("ErrMaxStack raised in %q, want runtime_test.lateRecurse; stack:\n%v", got, stack)
	}
	if lateReturns != lateLevels+1 {
		t.Errorf("lateHelper returned %d times in %d levels, want each call completed", lateReturns, lateLevels+1)
	}
	// The growth past the limit comes where lateLeaf's frame no longer
	// fits, and the refusal as lateHelper returns, at the same level.
	if used := lateLevels * lateFrame; used < limit/2 || used > limit {
		t.Errorf("recursion stopped at %d levels, about %d bytes of stack; want about %d to %d", lateLevels, used, limit/2, limit)
	}
}

// TestGoplusMaxStackLateWalks walks the stack while a refusal is pending,
// as the garbage collector, a stack copy and runtime.Callers do. Each must
// see the caller whose return address the refusal overwrote.
func TestGoplusMaxStackLateWalks(t *testing.T) {
	needMaxStack(t)
	var walks [][]string
	moved, onStack := false, false
	lateCheck = func() {
		walks = append(walks, callerNames(0))
		walks = append(walks, fpCallerNames())
		runtime.GC()
		walks = append(walks, callerNames(0))
		var local int
		before := uintptr(unsafe.Pointer(&local))
		runtime.GCTestMoveStackOnNextCall()
		walks = append(walks, callerNames(0))
		moved = uintptr(unsafe.Pointer(&local)) != before
		onStack = runtime.GoplusLateRefusalOnStack()
		runtime.GC()
	}
	defer func() { lateCheck = nil }()
	p, _, _ := runLate(t, 1<<20)
	if err, ok := p.(error); !ok || !errors.Is(err, goplus.ErrMaxStack) {
		t.Fatalf("Run panicked with %v (%T), want goplus.ErrMaxStack", p, p)
	}
	if len(walks) != 4 {
		t.Fatalf("lateCheck ran %d walks, want 4: no refusal was pending in lateLeaf", len(walks))
	}
	if !moved {
		t.Errorf("the stack did not move while the refusal was pending")
	} else if !onStack {
		t.Errorf("the overwritten return address was not moved with the stack")
	}
	for i, stack := range walks {
		if got := below(stack, "runtime_test.lateHelper"); got != "runtime_test.lateRecurse" {
			t.Errorf("walk %d: lateHelper called by %q, want runtime_test.lateRecurse; stack:\n%v", i, got, stack)
		}
		if !slices.Contains(stack, "goplus.(*AllocDomain).Run") {
			t.Errorf("walk %d stops short of Run; stack:\n%v", i, stack)
		}
	}
}

// TestGoplusMaxStackLateHeld checks a growth past the limit while the
// runtime code that called it holds a lock, as getitab does under itabLock
// and chansend under the channel's lock, and that releases it before it
// returns. The growth cannot be refused in place, and the refusal waits
// for the runtime to return as for any other runtime call: by then the
// lock is released and the goroutine can take the panic.
func TestGoplusMaxStackLateHeld(t *testing.T) {
	needMaxStack(t)
	lateHold = true
	defer func() { lateHold = false }()
	p, stack, late := runLate(t, 1<<20)
	if err, ok := p.(error); !ok || !errors.Is(err, goplus.ErrMaxStack) {
		t.Fatalf("Run panicked with %v (%T), want goplus.ErrMaxStack", p, p)
	}
	if late == 0 {
		t.Fatalf("no refusal waited for a runtime call; the test does not test that")
	}
	if got := below(stack, "runtime.goplusPanicMaxStack"); got != "runtime_test.lateRecurse" {
		t.Errorf("ErrMaxStack raised in %q, want runtime_test.lateRecurse; stack:\n%v", got, stack)
	}
}

// TestGoplusMaxStackLateOwnPanic checks that a runtime function that
// panics on its own while a refusal waits for it to return raises its own
// panic, and that recovering it forgets the refusal.
func TestGoplusMaxStackLateOwnPanic(t *testing.T) {
	needMaxStack(t)
	lateOwnPanic = true
	defer func() { lateOwnPanic = false }()
	p, _, late := runLate(t, 1<<20)
	if p != "own" {
		t.Fatalf("Run panicked with %v (%T), want the helper's own panic", p, p)
	}
	if late == 0 {
		t.Fatalf("no refusal waited for a runtime call; the test does not test that")
	}
}

// lockHelper is lateHelper with results in registers of each kind, and
// once, while a refusal is pending, it returns holding the M's lock count
// up, as a runtime function that takes a lock for its caller would.
//
//go:noinline
func lockHelper(n int) (int, float64, string, bool) {
	r := lateLeaf(n)
	if !lockHeld && runtime.GoplusLateRefusalPending() {
		lockHeld = true
		runtime.Acquirem()
		return n*3 + r - r, float64(n) / 2, "held", true
	}
	return n * 3, float64(n) / 2, "", false
}

const lockHelperName = "runtime_test.lockHelper"

var (
	lockHeld bool
	lockBad  string
)

//go:noinline
func lockRecurse(n int) int {
	var buf [lateFrame - 64]byte
	buf[n%len(buf)] = byte(n)
	lateLevels = n
	i, f, s, held := lockHelper(n)
	if held {
		runtime.Releasem()
		if i != n*3 || f != float64(n)/2 || s != "held" {
			lockBad = fmt.Sprintf("lockHelper(%d) returned %d, %v, %q; want %d, %v, %q", n, i, f, s, n*3, float64(n)/2, "held")
		}
	}
	return i + lockRecurse(n+1) + int(buf[(n+1)%len(buf)])
}

// TestGoplusMaxStackLateCannotPanic checks that a refusal waiting for a
// runtime call that returns holding a lock is dropped, with the call's
// results intact, and that the goroutine's next growth is refused.
func TestGoplusMaxStackLateCannotPanic(t *testing.T) {
	needMaxStack(t)
	lockHeld, lockBad = false, ""
	p, _, late := runLateWith(t, 1<<20, lockHelperName, func() { lockRecurse(0) })
	if err, ok := p.(error); !ok || !errors.Is(err, goplus.ErrMaxStack) {
		t.Fatalf("Run panicked with %v (%T), want goplus.ErrMaxStack", p, p)
	}
	if !lockHeld {
		t.Fatalf("lockHelper never returned holding a lock with a refusal pending")
	}
	if lockBad != "" {
		t.Error(lockBad)
	}
	if late < 2 {
		t.Errorf("%d refusals waited for a runtime call, want the dropped one and the next", late)
	}
}

// padStack calls f padStack levels deeper than its caller. Each level is
// a small frame, so a scan over n moves where on the stack f runs in
// small steps.
//
//go:noinline
func padStack(n int, f func()) {
	if n > 0 {
		padStack(n-1, f)
		return
	}
	f()
}

// scanLate runs op at a range of depths, each on a new goroutine inside
// Run on a domain whose limit refuses the first growth, so that across the
// scan the growth comes at each place in op's runtime calls in turn. op
// reports whether it saw a refusal, and whether that refusal was the one
// the test looks for. scanLate returns at how many depths it was, and how
// many refusals waited for a runtime call.
//
// The range starts at the shallowest depth where op is refused, and ends
// at the first depth where op is refused before it starts: a goroutine's
// first stack is not a fixed size, since the runtime adapts it to the
// stacks goroutines have used. The garbage collector is off during the
// scan: a collection can change that size, shrink a stack while op runs,
// or make an allocation take a deeper path, and each of those moves the
// range off op (seen in all.bash on arm64 runners).
func scanLate(t *testing.T, op func() (refused, mark bool)) (marks int, late uint64) {
	needMaxStack(t)
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	old := debug.SetMaxStack(64 << 20)
	defer debug.SetMaxStack(old)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(0)
	entered := false
	at := func(n int) (refused, mark bool) {
		done := make(chan struct{})
		entered = false
		go func() {
			defer close(done)
			defer func() {
				if recover() != nil {
					refused = true
				}
			}()
			d.Run(func() { padStack(n, func() { entered = true; refused, mark = op() }) })
		}()
		<-done
		return refused, mark
	}
	// Refusal is monotonic in depth: find where it starts.
	hi := 64
	for refused, _ := at(hi); !refused; refused, _ = at(hi) {
		if hi > 1<<20 {
			t.Fatalf("op is not refused at depth %d", hi)
		}
		hi *= 2
	}
	lo := 0
	for lo < hi {
		if mid := (lo + hi) / 2; func() bool { r, _ := at(mid); return r }() {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	// Scan on until op is refused before it starts. A call op makes can
	// take a deeper path in one run than in the next (an allocation that
	// refills its cache), which only moves lo shallower, so the scan
	// still reaches each place in op.
	before := runtime.GoplusLateRefusals()
	for n := lo; ; n++ {
		if _, mark := at(n); mark {
			marks++
		}
		if !entered {
			break
		}
		if n > lo+1<<16 {
			t.Fatalf("op is still entered at depth %d, past where it is first refused at %d", n, lo)
		}
	}
	return marks, runtime.GoplusLateRefusals() - before
}

// deferInLoop registers its deferred call with runtime.deferproc, which
// records where in deferInLoop to resume if the call recovers. It calls
// nothing else before, so that a growth there comes in deferproc's calls.
// It reports the panic the deferred call recovered.
//
//go:noinline
func deferInLoop() any {
	deferRecovered = nil
	for range 1 {
		defer recoverDeferred()
	}
	return nil
}

var deferRecovered any

func recoverDeferred() { deferRecovered = recover() }

// TestGoplusMaxStackLateDefer checks a refusal raised as deferproc returns.
// deferproc reads its return address after it allocates, which can grow
// the stack, and keeps it for recovery to resume at; the refusal must not
// leave goplusHijackReturn there ("no deferreturn").
func TestGoplusMaxStackLateDefer(t *testing.T) {
	marks, late := scanLate(t, func() (refused, mark bool) {
		deferInLoop()
		err, _ := deferRecovered.(error)
		// Recovered in deferInLoop: the deferred call was registered
		// before the panic, which only a refusal as deferproc returned
		// raises there. Other refusals unwind out of deferInLoop.
		mark = errors.Is(err, goplus.ErrMaxStack)
		return mark, mark
	})
	t.Logf("%d refusals as deferproc returned; %d refusals waited for a runtime call", marks, late)
	if marks == 0 {
		t.Fatalf("no refusal came as deferproc returned; the scan does not test that")
	}
}

var mapKeys = func() []string {
	var keys []string
	for i := range 64 {
		keys = append(keys, fmt.Sprint("key", i))
	}
	return keys
}()

// assignAll assigns each of mapKeys in a new map, growing it. It reports
// whether that was refused, and whether the refusal came after the
// assignment it interrupted, which leaves the key in the map with a zero
// value; and it fails the test if the map is not usable afterwards.
//
//go:noinline
func assignAll(t *testing.T) (refused, late bool) {
	m := make(map[string]int)
	i := 0
	defer func() {
		if recover() == nil {
			return
		}
		refused = true
		v, ok := m[mapKeys[i]]
		late = ok && v == 0
		for j, k := range mapKeys {
			m[k] = j + 1
		}
		if len(m) != len(mapKeys) {
			t.Errorf("map has %d keys after the refusal, want %d", len(m), len(mapKeys))
		}
	}()
	for i = range mapKeys {
		m[mapKeys[i]] = i + 1
	}
	return false, false
}

// TestGoplusMaxStackLateMap checks refusals that wait for a map assignment
// to return: the map stays usable, unlike after a panic in the middle of
// the assignment, which leaves the map marked as being written.
func TestGoplusMaxStackLateMap(t *testing.T) {
	marks, late := scanLate(t, func() (bool, bool) { return assignAll(t) })
	t.Logf("%d refusals as a map assignment returned; %d refusals waited for a runtime call", marks, late)
	if marks == 0 {
		t.Fatalf("no refusal came as a map assignment returned; the scan does not test that")
	}
}

// parkedCoroutines returns how many goroutines are coroutines waiting to
// be switched to, as iter.Pull's is until its next or stop is called.
func parkedCoroutines() int {
	buf := make([]byte, 16<<20)
	return bytes.Count(buf[:runtime.Stack(buf, true)], []byte(" [coroutine]:"))
}

// TestGoplusMaxStackLatePull checks that a refusal does not wait for
// runtime.newcoro, which iter.Pull calls by name: raised as newcoro
// returned, it would come before iter.Pull returned the next and stop
// functions that alone can end the coroutine newcoro made, which would
// stay parked forever.
//
// A refusal at one of the allocations iter.Pull makes after newcoro
// returns leaks the coroutine too (#106). That is not what this test
// checks, so a scan that saw one is run again.
func TestGoplusMaxStackLatePull(t *testing.T) {
	last := newcoroLine(t)
	for attempt := 1; ; attempt++ {
		before := parkedCoroutines()
		inPull, after := 0, 0
		scanLate(t, func() (refused, mark bool) {
			defer func() {
				if p := recover(); p != nil {
					if fn, line := frameBelow("runtime.goplusPanicMaxStack"); strings.HasPrefix(fn, "iter.Pull[") {
						if line > last {
							after++
						} else {
							inPull++
						}
					}
					panic(p)
				}
			}()
			next, stop := iter.Pull(func(yield func(int) bool) { yield(1) })
			defer stop()
			next()
			return false, false
		})
		if after > 0 {
			t.Logf("%d refusals came after newcoro returned (#106)", after)
			if attempt == 5 {
				t.Fatalf("each of %d scans had a refusal after newcoro returned", attempt)
			}
			continue
		}
		if inPull == 0 {
			t.Fatalf("no refusal came in iter.Pull; the scan does not test that")
		}
		if n := parkedCoroutines() - before; n > 0 {
			t.Errorf("%d coroutines left parked by %d refusals in iter.Pull", n, inPull)
		}
		return
	}
}

// newcoroLine returns the last line of the statement in iter.Pull that
// calls newcoro.
func newcoroLine(t *testing.T) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../iter/iter.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Pull" {
			for _, st := range fd.Body.List {
				if as, ok := st.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
					if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newcoro" {
							return fset.Position(as.End()).Line
						}
					}
				}
			}
		}
	}
	t.Fatal("iter.Pull no longer assigns the result of newcoro")
	return 0
}

// frameBelow returns the function and line of the caller of the frame of
// the named function on the calling goroutine's stack.
func frameBelow(name string) (string, int) {
	pcs := make([]uintptr, 1<<10)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for found := false; ; {
		f, more := frames.Next()
		if found {
			return f.Function, f.Line
		}
		found = f.Function == name
		if !more {
			return "", 0
		}
	}
}
