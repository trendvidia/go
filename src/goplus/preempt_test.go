// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"fmt"
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
	// Skip here, not when startSpinners maps the code: that comes after
	// the compiled baseline, which may never stop on such a platform.
	if code, _, _ := spinCode(0, 0, 0, true); code == nil || !execmem.Supported() {
		t.Skipf("no generated-code support on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const procs = 4
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))

	// Only preemption signals stop the compiled loops, so without them a
	// collection would never return. The polling loops need no signals.
	compiled := "not measured with asynchronous preemption off"
	if !asyncPreemptOff() {
		compiled = fmt.Sprintf("under %gs", measureStops(t, "compiled loops", startCompiledSpinners(procs)))
	}
	generated := measureStops(t, "generated loops", startSpinners(t, procs, true))
	t.Logf("slowest stop-the-world with %d spinning goroutines: compiled loops %s, polling generated loops under %gs", procs, compiled, generated)
	// The control shows unpolled loops never stop. Polled ones must stop
	// in bounded time; the bound allows for a loaded machine.
	if generated > 1 {
		t.Errorf("stopping the world took up to %gs with polling generated loops on every P", generated)
	}
}

// spinBatch is how many iterations a compiled spinner runs between
// touches of the shared words.
const spinBatch = 1024

// asyncPreemptOff reports whether GODEBUG turns off signal-based
// preemption: asyncpreemptoff=1 does, and so does gccheckmark=1 (see
// parseRuntimeDebugVars).
func asyncPreemptOff() bool {
	godebug := os.Getenv("GODEBUG")
	return strings.Contains(godebug, "asyncpreemptoff=1") || strings.Contains(godebug, "gccheckmark=1")
}

// startCompiledSpinners starts n goroutines spinning in compiled Go, the
// baseline for TestPreemptPoll. The loop makes no calls except, under
// -race, the atomics it runs once every spinBatch iterations.
func startCompiledSpinners(n int) (stop func()) {
	atomic.StoreUint32(&spinStop, 0)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Count locally and touch the shared words once a batch:
			// under -race each atomic is a call into the race runtime,
			// where a preemption signal cannot stop the goroutine, and a
			// loop made only of them kept the world from stopping for
			// minutes. With plain Go in between, almost every signal
			// lands where the loop can be preempted.
			for i := uint64(1); ; i++ {
				if i%spinBatch == 0 {
					atomic.AddUint64(&spinCounter, spinBatch)
					if atomic.LoadUint32(&spinStop) != 0 {
						return
					}
				}
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
func measureStops(t *testing.T, what string, stop func()) float64 {
	t.Helper()
	defer stop()
	atomic.StoreUint64(&spinCounter, 0)
	waitCounter(t, what) // a positive marker that the loops are running

	stopping := []metrics.Sample{{Name: "/sched/pauses/stopping/gc:seconds"}}
	metrics.Read(stopping)
	before := slices.Clone(stopping[0].Value.Float64Histogram().Counts)
	for range 5 {
		progress := atomic.LoadUint64(&spinCounter)
		runtime.GC()
		if atomic.LoadUint64(&spinCounter) == progress {
			t.Fatalf("the %s made no progress across a collection", what)
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

// waitCounter waits for the spinning loops, which what names, to count.
func waitCounter(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadUint64(&spinCounter) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the %s never ran", what)
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
	waitCounter(t, "generated loops")
	println("loops running")
	runtime.GC()
	println("GC returned")
}
