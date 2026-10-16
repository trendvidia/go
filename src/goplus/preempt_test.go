// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"goplus"
	"goplus/internal/execmem"
	"internal/testenv"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// What the spin loops read and write.
var (
	spinCounter uint64
	spinStop    uint32
)

// startSpinners registers and starts n goroutines running spinCode's
// loop, with or without the preemption poll, and returns a function that
// stops them and waits for them to return.
func startSpinners(t *testing.T, n int, poll bool) (stop func()) {
	t.Helper()
	code, frame, safe := spinCode(uintptr(unsafe.Pointer(&spinCounter)), uintptr(unsafe.Pointer(&spinStop)), goplus.YieldEntry(), poll)
	c := mapCode(t, code)
	f := goplus.Func{Name: "goplus_test.spin", Entry: c.Addr(), End: c.Addr() + uintptr(len(code)), Frame: frame}
	if poll {
		f.SafePoints = []goplus.SafePoint{{Off: safe}}
	}
	r, err := goplus.RegisterCode([]goplus.Func{f})
	if err != nil {
		t.Fatal(err)
	}
	spin := execmem.Func[func()](c)
	atomic.StoreUint32(&spinStop, 0)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spin()
		}()
	}
	return func() {
		atomic.StoreUint32(&spinStop, 1)
		wg.Wait()
		if err := r.Unregister(); err != nil {
			t.Errorf("Unregister: %v", err)
		}
	}
}

// TestPreemptPoll runs a generated loop with no calls on every P and
// checks that the world can still be stopped: the loops' polls are their
// only way to stop. For context it measures ordinary compiled loops the
// same way, which the runtime stops with preemption signals.
func TestPreemptPoll(t *testing.T) {
	if os.Getenv("GOPLUS_SPIN_CHILD") != "" {
		t.Skip("in child")
	}
	const procs = 4
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))

	compiled := measureStops(t, startCompiledSpinners(procs))
	generated := measureStops(t, startSpinners(t, procs, true))
	t.Logf("slowest stop-the-world with %d spinning goroutines: compiled loops under %gs, polling generated loops under %gs", procs, compiled, generated)
	// The control shows unpolled loops never stop. Polled ones must stop
	// in bounded time; the bound allows for a loaded machine.
	if generated > 1 {
		t.Errorf("stopping the world took up to %gs with polling generated loops on every P", generated)
	}
}

// startCompiledSpinners starts n goroutines spinning in compiled Go with
// no calls, the baseline for TestPreemptPoll.
func startCompiledSpinners(n int) (stop func()) {
	atomic.StoreUint32(&spinStop, 0)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for atomic.LoadUint32(&spinStop) == 0 {
				spinCounter++
			}
		}()
	}
	return func() {
		atomic.StoreUint32(&spinStop, 1)
		wg.Wait()
	}
}

// measureStops runs five collections while the spinners run, stops them,
// and returns the upper bound of the slowest recorded time to stop the
// world for a collection.
func measureStops(t *testing.T, stop func()) float64 {
	t.Helper()
	defer stop()
	atomic.StoreUint64(&spinCounter, 0)
	waitCounter(t) // a positive marker that the loops are running

	stopping := []metrics.Sample{{Name: "/sched/pauses/stopping/gc:seconds"}}
	metrics.Read(stopping)
	before := slices.Clone(stopping[0].Value.Float64Histogram().Counts)
	for range 5 {
		progress := atomic.LoadUint64(&spinCounter)
		runtime.GC()
		if atomic.LoadUint64(&spinCounter) == progress {
			t.Fatal("the loops made no progress across a collection")
		}
	}
	metrics.Read(stopping)
	after := stopping[0].Value.Float64Histogram()
	stops, worst := 0, 0.0
	for i, n := range after.Counts {
		if d := n - before[i]; d > 0 {
			stops += int(d)
			worst = max(worst, after.Buckets[i+1])
		}
	}
	// Each collection stops the world twice.
	if stops < 10 {
		t.Fatalf("only %d stop-the-world pauses recorded for 5 collections", stops)
	}
	return worst
}

func waitCounter(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadUint64(&spinCounter) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the generated loops never ran")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPreemptPollControl runs the same loop without the poll, in a child
// process, and checks that a collection then never completes. It shows
// the poll is what lets TestPreemptPoll pass.
func TestPreemptPollControl(t *testing.T) {
	if os.Getenv("GOPLUS_SPIN_CHILD") != "" {
		t.Skip("in child")
	}
	if code, _, _ := spinCode(0, 0, 0, false); code == nil {
		t.Skip("no generated code on this architecture")
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestPreemptPollControlChild$", "-test.v")
	cmd.Env = append(os.Environ(), "GOPLUS_SPIN_CHILD=1")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	cmd.Process.Kill()
	cmd.Wait()
	got := out.String()
	if !strings.Contains(got, "loops running") {
		t.Fatalf("control child did not get its loops running:\n%s", got)
	}
	if strings.Contains(got, "GC returned") {
		t.Fatalf("a collection completed with unpolled generated loops running; the control shows nothing:\n%s", got)
	}
}

func TestPreemptPollControlChild(t *testing.T) {
	if os.Getenv("GOPLUS_SPIN_CHILD") == "" {
		t.Skip("run by TestPreemptPollControl")
	}
	// Leave one P free so this goroutine can call runtime.GC.
	const procs = 4
	runtime.GOMAXPROCS(procs)
	startSpinners(t, procs-1, false)
	waitCounter(t)
	println("loops running")
	runtime.GC()
	println("GC returned")
}
