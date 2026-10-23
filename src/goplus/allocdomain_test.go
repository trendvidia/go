// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus_test

import (
	"errors"
	"fmt"
	"goplus"
	"internal/testenv"
	"iter"
	"math"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// allocSinks keeps allocations reachable, one slot per worker so that
// concurrent workers do not write the same variable. The slots are typed:
// storing a slice in an interface would allocate its header too.
var allocSinks [64][]byte

// allocExact allocates n pointer-free objects of size bytes, as worker w.
// Sizes used with it are exact size classes (or whole pages, for large
// objects), so the bytes requested equal the bytes the allocator hands
// out, and it allocates nothing else.
//
//go:noinline
func allocExact(w, n, size int) {
	for range n {
		allocSinks[w] = make([]byte, size)
	}
}

// ptrSinks is allocSinks for objects that contain pointers.
var ptrSinks [64][]*byte

// allocPointers is allocExact for objects of n pointers.
//
//go:noinline
func allocPointers(w, count, n int) {
	for range count {
		ptrSinks[w] = make([]*byte, n)
	}
}

// TestAllocDomainSmall checks the count for each kind of small
// allocation the runtime has a size-specialised function for: tiny,
// pointer-free and with pointers, and tiny with SetNoTinyAlloc, whose
// allocations the tiny function hands to another. Each must be counted
// once, by whichever function finally allocates it.
//
// TestAllocDomainSlowPath runs this test again with every allocation
// sent down the slow path.
func TestAllocDomainSmall(t *testing.T) {
	const count = 10000
	ptrSize := int(unsafe.Sizeof(uintptr(0)))
	for _, tc := range []struct {
		name   string
		noTiny bool
		bytes  int
		alloc  func()
	}{
		{"tiny", false, 8, func() { allocExact(0, count, 8) }},
		{"NoTinyAlloc", true, 8, func() { allocExact(0, count, 8) }},
		{"noscan", false, 64, func() { allocExact(0, count, 64) }},
		{"scan", false, 4 * ptrSize, func() { allocPointers(0, count, 4) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noTiny {
				defer goplus.SetNoTinyAlloc(goplus.SetNoTinyAlloc(true))
			}
			d := goplus.NewAllocDomain()
			d.Run(tc.alloc)
			want := uint64(count * tc.bytes)
			if n := d.Allocated(); n < want || n > want+want/100 {
				t.Fatalf("domain counted %d bytes, want %d within 1%% (%.2fx)", n, want, float64(n)/float64(want))
			}
		})
	}
}

// TestAllocDomainSlowPath reruns the counting and budget tests in a
// process where every allocation takes the allocator's slow path, as it
// does whenever the collector is marking. GODEBUG=inittrace=1 turns on the
// allocator's debug hooks, which force that path.
func TestAllocDomainSlowPath(t *testing.T) {
	testenv.MustHaveExec(t)
	cmd := testenv.CleanCmdEnv(exec.Command(testenv.Executable(t),
		"-test.run=^(TestAllocDomainSmall|TestAllocDomainMatchesMemStats|TestAllocBudgetSmall|TestAllocBudgetStartedGoroutine|TestAllocBudgetPull)$", "-test.v", "-test.count=1"))
	cmd.Env = append(cmd.Environ(), "GODEBUG=inittrace=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Require each test to have run, not merely nothing to have failed.
	for _, name := range []string{"TestAllocDomainSmall", "TestAllocDomainMatchesMemStats", "TestAllocBudgetSmall", "TestAllocBudgetStartedGoroutine", "TestAllocBudgetPull"} {
		if !strings.Contains(string(out), "--- PASS: "+name+" ") {
			t.Errorf("%s did not pass in the child process:\n%s", name, out)
		}
	}
}

// TestAllocDomainMatchesMemStats compares a single domain's count with
// the growth of MemStats.TotalAlloc over the same work.
func TestAllocDomainMatchesMemStats(t *testing.T) {
	d := goplus.NewAllocDomain()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d.Run(func() {
		allocExact(0, 20000, 64)
		allocExact(0, 2000, 4096)
		allocExact(0, 20, 1<<20)
	})
	runtime.ReadMemStats(&after)
	const want = 20000*64 + 2000*4096 + 20*(1<<20)
	got := d.Allocated()
	total := after.TotalAlloc - before.TotalAlloc
	t.Logf("domain counted %d bytes; TotalAlloc grew by %d; the work requested %d", got, total, want)
	// The domain counts the work plus whatever else this goroutine
	// allocated meanwhile, which is small.
	if got < want || got > want+want/100 {
		t.Errorf("domain counted %d bytes, want %d within 1%%", got, want)
	}
	// TotalAlloc also counts other goroutines' allocations; with exact
	// size classes the domain's share must match it to within 1%.
	if diff := int64(total) - int64(got); diff < -int64(want/100) || diff > int64(want/100) {
		t.Errorf("TotalAlloc grew by %d, domain counted %d: they differ by more than 1%%", total, got)
	}
}

func TestAllocDomainOutside(t *testing.T) {
	d := goplus.NewAllocDomain()
	allocExact(0, 1000, 64)
	done := make(chan bool)
	go func() { allocExact(1, 1000, 64); done <- true }()
	<-done
	if n := d.Allocated(); n != 0 {
		t.Fatalf("domain counted %d bytes for allocations made outside it", n)
	}
	d.Run(func() {})
	allocExact(0, 1000, 64) // after Run returned: outside again
	if n := d.Allocated(); n > 4096 {
		t.Fatalf("domain counted %d bytes after Run returned", n)
	}
}

// TestAllocDomainInherited checks that goroutines started in a domain
// count in it, even when they allocate after Run has returned, and that
// their own children inherit it too.
func TestAllocDomainInherited(t *testing.T) {
	d := goplus.NewAllocDomain()
	release := make(chan bool)
	done := make(chan bool)
	d.Run(func() {
		go func() {
			<-release
			allocExact(1, 1000, 4096)
			go func() {
				allocExact(2, 1000, 4096)
				done <- true
			}()
		}()
	})
	if n := d.Allocated(); n > 4096 {
		t.Fatalf("domain counted %d bytes before its goroutine allocated", n)
	}
	close(release)
	<-done
	if n, want := d.Allocated(), uint64(2*1000*4096); n < want {
		t.Fatalf("domain counted %d bytes, want at least %d from its goroutine and grandchild", n, want)
	}
}

func TestAllocDomainNested(t *testing.T) {
	outer, inner := goplus.NewAllocDomain(), goplus.NewAllocDomain()
	outer.Run(func() {
		allocExact(0, 1000, 64)
		inner.Run(func() { allocExact(0, 1000, 4096) })
		allocExact(0, 1000, 64)
	})
	if n := inner.Allocated(); n < 1000*4096 || n > 1000*4096+4096 {
		t.Errorf("inner domain counted %d bytes, want about %d", n, 1000*4096)
	}
	if n := outer.Allocated(); n < 2*1000*64 || n >= 1000*4096 {
		t.Errorf("outer domain counted %d bytes, want about %d and none of the inner work", n, 2*1000*64)
	}
}

// TestAllocDomainConcurrent allocates from many goroutines of one domain
// on every P and checks the total.
func TestAllocDomainConcurrent(t *testing.T) {
	const workers, perWorker, size = 32, 2000, 128
	d := goplus.NewAllocDomain()
	var wg sync.WaitGroup
	d.Run(func() {
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allocExact(w, perWorker, size)
			}()
		}
	})
	wg.Wait()
	want := uint64(workers * perWorker * size)
	if n := d.Allocated(); n < want || n > want+want/100 {
		t.Fatalf("domain counted %d bytes, want %d within 1%%", n, want)
	}
}

// TestAllocDomainReusedGoroutine checks that a goroutine started outside
// any domain is not counted even when it reuses the record of a goroutine
// that ran, and exited, inside one. Goroutine records go on a per-P free
// list when a goroutine exits and the next goroutine started on that P
// takes one from it, so starting goroutines on one P right after a batch
// exited makes reuse near certain.
func TestAllocDomainReusedGoroutine(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const n = 64
	d := goplus.NewAllocDomain()
	var wg sync.WaitGroup
	d.Run(func() {
		for w := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allocExact(w, 10, 64) // each goroutine its own sink slot
			}()
		}
	})
	wg.Wait()
	runtime.Gosched() // let the exited goroutines' records reach the free list
	before := d.Allocated()
	if before < n*10*64 {
		t.Fatalf("domain counted %d bytes from its own goroutines, want at least %d", before, n*10*64)
	}
	for w := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allocExact(w, 10, 4096)
		}()
	}
	wg.Wait()
	if after := d.Allocated(); after != before {
		t.Fatalf("domain grew from %d to %d bytes from goroutines started outside it (stale domain on a reused goroutine record)", before, after)
	}
}

// TestAllocDomainNotRetained checks that goroutines that ran in a domain
// do not keep it alive once they exit. Their records stay in the
// runtime, on a free list, until they are reused.
func TestAllocDomainNotRetained(t *testing.T) {
	wp := runDomainAndDrop(t)
	deadline := time.Now().Add(10 * time.Second)
	for wp.Value() != nil {
		if time.Now().After(deadline) {
			t.Fatal("domain still reachable after its goroutines exited and the domain was dropped")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

// runDomainAndDrop runs goroutines in a new domain, waits for them to
// exit, and returns a weak pointer to the domain's counter.
//
//go:noinline
func runDomainAndDrop(t *testing.T) weak.Pointer[byte] {
	const n = 64
	d := goplus.NewAllocDomain()
	var wg sync.WaitGroup
	d.Run(func() {
		for w := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allocExact(w, 10, 64)
			}()
		}
	})
	wg.Wait()
	if got := d.Allocated(); got < n*10*64 {
		t.Fatalf("domain counted %d bytes from its goroutines, want at least %d", got, n*10*64)
	}
	return weak.Make((*byte)(goplus.AllocDomainCounter(d)))
}

// TestAllocDomainZero checks that the zero AllocDomain, which is not a
// domain, panics instead of silently taking the goroutine out of the
// domain it is in.
func TestAllocDomainZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func(*goplus.AllocDomain)
	}{
		{"Run", func(d *goplus.AllocDomain) { d.Run(func() {}) }},
		{"Allocated", func(d *goplus.AllocDomain) { d.Allocated() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if s, _ := r.(string); !strings.Contains(s, "NewAllocDomain") {
					t.Fatalf("got panic %v, want one naming NewAllocDomain", r)
				}
			}()
			tc.f(new(goplus.AllocDomain))
		})
	}
}

// BenchmarkAllocDomain measures an allocation inside a domain against
// one outside.
func BenchmarkAllocDomain(b *testing.B) {
	b.Run("outside", func(b *testing.B) {
		for b.Loop() {
			allocSinks[0] = make([]byte, 64)
		}
	})
	b.Run("inside", func(b *testing.B) {
		goplus.NewAllocDomain().Run(func() {
			for b.Loop() {
				allocSinks[0] = make([]byte, 64)
			}
		})
	})
}

// budgetGranule is the runtime's goplusBudgetGranule: small allocations
// are checked against the budget each time a P's count crosses a
// multiple of it.
const budgetGranule = 64 << 10

// runRefused runs f in d and returns the value it panicked with, or nil.
func runRefused(d *goplus.AllocDomain, f func()) (r any) {
	d.Run(func() {
		defer func() { r = recover() }()
		f()
	})
	return r
}

// checkBudgetPanic fails t unless r is the budget refusal.
func checkBudgetPanic(t *testing.T, r any) {
	t.Helper()
	if r == nil {
		t.Fatal("allocation past the budget was not refused")
	}
	err, ok := r.(error)
	if !ok || !errors.Is(err, goplus.ErrAllocBudget) {
		t.Fatalf("panicked with %v (%T), want goplus.ErrAllocBudget", r, r)
	}
	if _, ok := r.(runtime.Error); !ok {
		t.Fatalf("panic value %T is not a runtime.Error", r)
	}
}

// TestAllocBudgetHugeRequest asks a native that sizes its own result,
// strings.Repeat, for far more memory than the budget, and more than
// the machine has on 64-bit platforms. Without the budget this is
// "fatal error: out of memory", which no recover catches.
func TestAllocBudgetHugeRequest(t *testing.T) {
	d := goplus.NewAllocDomain()
	d.SetBudget(64 << 20)
	// 192 TiB on 64-bit platforms, under the 256 TiB beyond which
	// makeslice already refuses a length; 1 GiB on 32-bit ones.
	const n = int(min(uint64(3)<<46, math.MaxInt>>1))
	var s string
	r := runRefused(d, func() { s = strings.Repeat("x", n) })
	checkBudgetPanic(t, r)
	if n := d.Allocated(); n > 1<<20 {
		t.Errorf("domain counted %d bytes after the refusal; the refused request must not count", n)
	}
	if s != "" {
		t.Fatalf("strings.Repeat returned %d bytes", len(s))
	}

	// The process and the domain carry on: an allocation within the
	// budget succeeds.
	if r := runRefused(d, func() { allocExact(0, 1, 1<<20) }); r != nil {
		t.Fatalf("allocation within the budget panicked: %v", r)
	}
	if n := d.Allocated(); n < 1<<20 {
		t.Errorf("domain counted %d bytes, want at least %d after the allocation within budget", n, 1<<20)
	}
}

// TestAllocBudgetSmall allocates small objects in a loop until one is
// refused, and checks the count against the stated overshoot: less than
// one granule per P.
func TestAllocBudgetSmall(t *testing.T) {
	const budget = 4 << 20
	for _, tc := range []struct {
		name  string
		size  int
		alloc func()
	}{
		{"tiny", 8, func() { allocExact(0, 1<<30, 8) }},
		{"noscan", 64, func() { allocExact(0, 1<<30, 64) }},
		{"scan", 4 * int(unsafe.Sizeof(uintptr(0))), func() { allocPointers(0, 1<<30, 4) }},
		{"medium", 16 << 10, func() { allocExact(0, 1<<30, 16<<10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := goplus.NewAllocDomain()
			d.SetBudget(budget)
			checkBudgetPanic(t, runRefused(d, tc.alloc))
			n := d.Allocated()
			over := uint64(runtime.GOMAXPROCS(0)) * budgetGranule
			t.Logf("refused at %d bytes, budget %d", n, budget)
			if n+uint64(tc.size) <= budget || n > budget+over {
				t.Fatalf("refused at %d bytes, want past %d by less than %d", n, budget, over)
			}
		})
	}
}

// TestAllocBudgetConcurrent runs allocating goroutines of one domain on
// every P and checks that each is refused, within the overshoot.
func TestAllocBudgetConcurrent(t *testing.T) {
	const workers, budget = 16, 8 << 20
	d := goplus.NewAllocDomain()
	d.SetBudget(budget)
	var wg sync.WaitGroup
	refused := make([]any, workers)
	// Each worker enters d itself: the budget refuses only a goroutine
	// inside a call to Run, not one that Run's function started.
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refused[w] = runRefused(d, func() { allocExact(w, 1<<30, 128) })
		}()
	}
	wg.Wait()
	for _, r := range refused {
		checkBudgetPanic(t, r)
	}
	over := uint64(runtime.GOMAXPROCS(0)) * budgetGranule
	if n := d.Allocated(); n <= budget-workers*128 || n > budget+over {
		t.Fatalf("domain counted %d bytes, want past %d by less than %d", n, budget, over)
	}
}

// TestAllocBudgetChange raises and removes the budget of a domain that
// is over it, and lowers one below what the domain has allocated.
func TestAllocBudgetChange(t *testing.T) {
	d := goplus.NewAllocDomain()
	d.SetBudget(1 << 20)
	big := func() { allocExact(0, 1, 1<<20) }
	if r := runRefused(d, big); r != nil {
		t.Fatalf("first 1 MiB within a 1 MiB budget panicked: %v", r)
	}
	checkBudgetPanic(t, runRefused(d, big))
	d.SetBudget(4 << 20) // grant more
	if r := runRefused(d, big); r != nil {
		t.Fatalf("allocation after raising the budget panicked: %v", r)
	}
	d.SetBudget(1 << 20) // lower it below what is allocated
	checkBudgetPanic(t, runRefused(d, big))
	d.SetBudget(math.MaxUint64) // no budget
	if r := runRefused(d, func() { allocExact(0, 8, 1<<20) }); r != nil {
		t.Fatalf("allocation with no budget panicked: %v", r)
	}
}

// TestAllocBudgetStartedGoroutine checks that the budget refuses only a
// goroutine that is inside a call to Run (trendvidia/go#84). A goroutine
// Run's function starts is counted in the domain but never refused,
// while Run runs and after it returns: it has no recover, and a refusal
// there used to end the process. When that goroutine calls Run itself,
// it is refused inside the call, and not after it, whether the refusal
// is recovered inside the call or unwinds out of it.
func TestAllocBudgetStartedGoroutine(t *testing.T) {
	const budget = 1 << 20
	// past allocates well past the budget, in small objects and in one
	// large one, which take different paths through the allocator.
	past := func(w int) {
		allocExact(w, 1000, 4096)
		allocExact(w, 1, 4<<20)
	}
	// unrefused runs f and returns what it panicked with, if anything.
	unrefused := func(f func()) (r any) {
		defer func() { r = recover() }()
		f()
		return nil
	}

	d := goplus.NewAllocDomain()
	d.SetBudget(budget)
	release := make(chan bool)
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll() // a failure before close(release) must not leak the goroutine
	during := make(chan any)
	after := make(chan [5]any, 1)
	var duringR any
	refusedCaller := runRefused(d, func() {
		// Started here, allocating only after Run has returned.
		go func() {
			<-release
			var rs [5]any
			rs[0] = unrefused(func() { past(2) })
			rs[1] = runRefused(d, func() { past(2) })
			rs[2] = unrefused(func() { past(2) })
			// The refusal unwinds out of Run, and is recovered by
			// Run's caller, as an embedder's gate around Run does.
			rs[3] = unrefused(func() { d.Run(func() { past(2) }) })
			rs[4] = unrefused(func() { past(2) })
			after <- rs
		}()
		// Started while Run runs, and done before it returns.
		go func() { during <- unrefused(func() { past(1) }) }()
		duringR = <-during
		// The caller itself is still refused, also once a nested Run
		// on another domain has returned to this one.
		goplus.NewAllocDomain().Run(func() { allocExact(0, 1, 64) })
		past(0)
	})
	if duringR != nil {
		t.Fatalf("goroutine started while Run ran was refused: %v", duringR)
	}
	checkBudgetPanic(t, refusedCaller)

	before := d.Allocated()
	releaseAll()
	var rs [5]any
	select {
	case rs = <-after:
	case <-time.After(time.Minute):
		t.Fatal("goroutine that outlived Run did not finish within a minute")
	}
	if rs[0] != nil {
		t.Fatalf("goroutine that outlived Run was refused: %v", rs[0])
	}
	checkBudgetPanic(t, rs[1]) // inside its own call to d.Run
	if rs[2] != nil {
		t.Fatalf("goroutine was still refused after its own Run returned: %v", rs[2])
	}
	checkBudgetPanic(t, rs[3]) // unwinding out of its own call to d.Run
	if rs[4] != nil {
		t.Fatalf("goroutine was still refused after a refusal unwound out of its own Run: %v", rs[4])
	}
	// Positive marker: it allocated past the budget, and was counted.
	if n, want := d.Allocated()-before, uint64(3*(1000*4096+4<<20)); n < want {
		t.Fatalf("domain counted %d bytes from the goroutine that outlived Run, want at least %d", n, want)
	}
}

// TestAllocBudgetBesideStartedGoroutine checks that a goroutine the
// budget never refuses, allocating in the domain at the same time, does
// not keep the budget from refusing the caller of Run: on one P, where
// the two share the stripe whose granules decide when the budget is
// checked, and on every P.
func TestAllocBudgetBesideStartedGoroutine(t *testing.T) {
	const budget = 1 << 20
	for _, procs := range []int{1, runtime.GOMAXPROCS(0)} {
		t.Run(fmt.Sprint("procs=", procs), func(t *testing.T) {
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
			d := goplus.NewAllocDomain()
			d.SetBudget(budget)
			stop, done := make(chan bool), make(chan bool)
			d.Run(func() {
				go func() {
					defer close(done)
					for {
						select {
						case <-stop:
							return
						default:
						}
						allocExact(1, 64, 1152)
					}
				}()
			})
			defer func() { close(stop); <-done }()
			// Start the caller with the domain already past its budget, so
			// that its first checked allocation must be refused.
			for d.Allocated() <= 2*budget {
				runtime.Gosched()
			}
			var own uint64
			r := runRefused(d, func() {
				for own < 1<<30 {
					allocSinks[0] = make([]byte, 128)
					own += 128
				}
			})
			checkBudgetPanic(t, r)
			// Its allocations are checked about every granule on each P, and
			// the other goroutine's take a share of the checks but not of
			// the caller's bytes between them. Eight granules a P is a
			// bound it should not come near.
			if limit := 8 * uint64(procs) * budgetGranule; own > limit {
				t.Fatalf("caller allocated %d bytes before its refusal, want at most %d", own, limit)
			}
			t.Logf("caller refused after %d bytes of its own", own)
		})
	}
}

// TestAllocBudgetPull checks the budget on an iterator that iter.Pull
// runs (trendvidia/go#88). Its body runs on a goroutine of its own, and
// iter.Pull hands that goroutine's panics to whoever called next or
// stop. So the body is refused exactly when that caller could be: it is
// inside Run on the domain counting the body.
func TestAllocBudgetPull(t *testing.T) {
	const budget = 1 << 20
	// past allocates well past the budget, in small objects and in one
	// large one, which take different paths through the allocator.
	past := func() {
		allocExact(3, 1000, 4096)
		allocExact(3, 1, 4<<20)
	}
	unrefused := func(f func()) (r any) {
		defer func() { r = recover() }()
		f()
		return nil
	}
	// pulled returns an iterator that allocates past the budget before
	// each value it yields, and once more when it is stopped.
	pulled := func() (next func() (int, bool), stop func()) {
		return iter.Pull(func(yield func(int) bool) {
			for i := 0; ; i++ {
				past()
				if !yield(i) {
					past()
					return
				}
			}
		})
	}
	drain := func(next func() (int, bool)) {
		for i := 0; i < 64; i++ {
			if _, ok := next(); !ok {
				return
			}
		}
	}

	t.Run("next inside Run", func(t *testing.T) {
		d := goplus.NewAllocDomain()
		d.SetBudget(budget)
		checkBudgetPanic(t, runRefused(d, func() {
			next, stop := pulled()
			defer stop()
			drain(next)
		}))
	})

	t.Run("stop inside Run", func(t *testing.T) {
		d := goplus.NewAllocDomain()
		checkBudgetPanic(t, runRefused(d, func() {
			next, stop := pulled()
			next()                     // no budget yet
			d.SetBudget(d.Allocated()) // nothing more may be allocated
			stop()                     // the body allocates once more
		}))
	})

	t.Run("nested Pull inside Run", func(t *testing.T) {
		d := goplus.NewAllocDomain()
		d.SetBudget(budget)
		checkBudgetPanic(t, runRefused(d, func() {
			outer, stop := iter.Pull(func(yield func(int) bool) {
				inner, stopInner := pulled()
				defer stopInner()
				for {
					v, ok := inner()
					if !ok || !yield(v) {
						return
					}
				}
			})
			defer stop()
			drain(outer)
		}))
	})

	t.Run("next after Run returns", func(t *testing.T) {
		// Created inside Run, resumed outside it: no caller of Run is
		// left to recover a refusal, so none may happen, and the process
		// carries on.
		d := goplus.NewAllocDomain()
		d.SetBudget(budget)
		var next func() (int, bool)
		var stop func()
		d.Run(func() { next, stop = pulled() })
		before := d.Allocated()
		if r := unrefused(func() { drain(next); stop() }); r != nil {
			t.Fatalf("iterator resumed outside Run was refused: %v", r)
		}
		// Positive marker: it allocated past the budget, and was counted.
		if n, want := d.Allocated()-before, uint64(64*(1000*4096+4<<20)); n < want {
			t.Fatalf("domain counted %d bytes from the iterator, want at least %d", n, want)
		}
	})

	t.Run("next inside another domain's Run", func(t *testing.T) {
		// The iterator stays in the domain it was created in, and the
		// caller's Run on another domain does not make it refusable.
		d := goplus.NewAllocDomain()
		d.SetBudget(budget)
		var next func() (int, bool)
		var stop func()
		d.Run(func() { next, stop = pulled() })
		defer stop()
		e := goplus.NewAllocDomain()
		e.SetBudget(budget)
		if r := runRefused(e, func() { drain(next) }); r != nil {
			t.Fatalf("iterator of one domain was refused inside another's Run: %v", r)
		}
		if n := e.Allocated(); n > budget {
			t.Fatalf("the caller's domain counted %d bytes of the iterator's", n)
		}
	})

	t.Run("body inside its own Run", func(t *testing.T) {
		// The body calls Run on e and yields from inside it. While the
		// body is parked there, its caller is not in e: the caller's
		// allocations are neither counted in e nor refused by it. When
		// the body allocates past e's budget, the refusal reaches the
		// caller of next.
		e := goplus.NewAllocDomain()
		e.SetBudget(budget)
		next, stop := iter.Pull(func(yield func(int) bool) {
			e.Run(func() {
				if !yield(0) {
					return
				}
				past()
				yield(1)
			})
		})
		defer stop()
		d := goplus.NewAllocDomain() // no budget
		var r1, r2 any
		var inE uint64
		d.Run(func() {
			next()
			inE = e.Allocated()
			r1 = unrefused(past)
			inE = e.Allocated() - inE
			r2 = unrefused(func() { next() })
		})
		if r1 != nil {
			t.Fatalf("caller was refused while the body was parked in e.Run: %v", r1)
		}
		if inE != 0 {
			t.Fatalf("e counted %d bytes of the caller's while the body was parked in e.Run", inE)
		}
		if d.Allocated() < 1000*4096+4<<20 {
			t.Fatalf("caller's domain counted %d bytes, want at least one past()", d.Allocated())
		}
		checkBudgetPanic(t, r2)
	})

	t.Run("refusal before the body's recover", func(t *testing.T) {
		// The body allocates before iter.Pull's recover is in place: its
		// yield closure escapes. A refusal there would never reach the
		// caller: next would return as if the sequence were empty, and
		// stop would die with "coroswitch on exited coro". Each attempt
		// starts a fresh domain with nothing left and fills it to j
		// 16-byte objects short of the first granule, so across the
		// sweep the granule is crossed at each of the allocations the
		// first next makes, in the caller and in the body.
		pulls := map[string]func() (started *bool, next func() bool, stop func()){
			"Pull": func() (*bool, func() bool, func()) {
				started := new(bool)
				next, stop := iter.Pull(func(yield func(int) bool) { *started = true; yield(1) })
				return started, func() bool { _, ok := next(); return ok }, stop
			},
			"Pull2": func() (*bool, func() bool, func()) {
				started := new(bool)
				next, stop := iter.Pull2(func(yield func(int, int) bool) { *started = true; yield(1, 2) })
				return started, func() bool { _, _, ok := next(); return ok }, stop
			},
		}
		for name, pull := range pulls {
			refused := 0
			for j := 1; j <= 256; j++ {
				d := goplus.NewAllocDomain()
				d.SetBudget(0)
				lost := false
				r := runRefused(d, func() {
					allocExact(0, budgetGranule/16-j, 16)
					started, next, stop := pull()
					if !next() && !*started {
						lost = true // calling stop would end the process
						return
					}
					stop()
				})
				if lost {
					t.Fatalf("%s: %d objects short of the granule, the body was refused where no caller could recover it", name, j)
				}
				if r != nil {
					checkBudgetPanic(t, r)
					refused++
				}
			}
			// Positive marker: the sweep did reach the granule.
			if refused == 0 {
				t.Fatalf("%s: no attempt crossed the granule", name)
			}
		}
	})

	t.Run("huge request", func(t *testing.T) {
		// The request iterator's body makes would end the process with
		// "fatal error: out of memory" if it were let through.
		d := goplus.NewAllocDomain()
		d.SetBudget(64 << 20)
		const n = int(min(uint64(3)<<46, math.MaxInt>>1))
		var s string
		checkBudgetPanic(t, runRefused(d, func() {
			next, stop := iter.Pull(func(yield func(int) bool) {
				s = strings.Repeat("x", n)
				yield(len(s))
			})
			defer stop()
			next()
		}))
		if s != "" {
			t.Fatalf("strings.Repeat returned %d bytes", len(s))
		}
	})
}

// TestAllocBudgetZero checks that a budget of 0 has nothing left, and
// that a new domain has no budget.
func TestAllocBudgetZero(t *testing.T) {
	big := func() { allocExact(0, 1, 1<<20) }

	// A new domain, never given a budget, refuses nothing.
	d := goplus.NewAllocDomain()
	if r := runRefused(d, func() { allocExact(0, 8, 1<<20) }); r != nil {
		t.Fatalf("allocation in a new domain panicked: %v", r)
	}

	// 0 on a domain that has allocated: the next checked one is refused.
	d.SetBudget(0)
	checkBudgetPanic(t, runRefused(d, big))

	// 0 on a domain that has allocated nothing: a large allocation is
	// checked on its own, and refused.
	d = goplus.NewAllocDomain()
	d.SetBudget(0)
	checkBudgetPanic(t, runRefused(d, big))
	if n := d.Allocated(); n != 0 {
		t.Fatalf("domain with a budget of 0 counted %d bytes, want 0: a refusal is not counted", n)
	}
}

// TestAllocBudgetMapGrowth grows a map in a domain that is over budget.
// Its growth must be let through: a refusal would leave the map marked
// as being written, and the map's next write would throw "concurrent map
// writes". The map's elements make each table large enough to be checked
// against the budget on its own, so the growth does reach the check.
func TestAllocBudgetMapGrowth(t *testing.T) {
	type elem [120]byte
	m := make(map[int64]elem)
	d := goplus.NewAllocDomain()
	d.SetBudget(1)
	r := runRefused(d, func() {
		for i := range int64(4096) {
			m[i] = elem{}
		}
	})
	if r != nil {
		t.Fatalf("map growth in a domain over budget panicked: %v", r)
	}
	// Positive marker: the growth was counted, so it went past the
	// budget, and its largest tables were big enough to be checked.
	if n := d.Allocated(); n < 4*budgetGranule {
		t.Fatalf("domain counted %d bytes of map growth, want at least %d", n, 4*budgetGranule)
	}
	// The domain's next allocation that can panic is refused.
	checkBudgetPanic(t, runRefused(d, func() { allocExact(0, 1, 1<<20) }))
	// And the map is still usable.
	m[-1] = elem{}
	if len(m) != 4097 {
		t.Fatalf("map has %d entries, want 4097", len(m))
	}
}

// TestAllocBudgetMapStructKey is TestAllocBudgetMapGrowth for a key type
// the fast map routines do not handle, whose hash and equality functions
// the compiler generates and the map calls while it is marked as being
// written, and then clears the map in the same domain (trendvidia/go#96).
func TestAllocBudgetMapStructKey(t *testing.T) {
	type key struct {
		s string
		i any
		n int64
	}
	type elem [120]byte
	m := make(map[key]elem)
	d := goplus.NewAllocDomain()
	d.SetBudget(1)
	keys := make([]key, 4096)
	for i := range keys {
		keys[i] = key{s: "k", i: i & 1, n: int64(i)}
	}
	r := runRefused(d, func() {
		for _, k := range keys {
			m[k] = elem{}
		}
		clear(m)
		for _, k := range keys[:100] {
			m[k] = elem{}
		}
	})
	if r != nil {
		t.Fatalf("map writes in a domain over budget panicked: %v", r)
	}
	if n := d.Allocated(); n < 4*budgetGranule {
		t.Fatalf("domain counted %d bytes of map growth, want at least %d", n, 4*budgetGranule)
	}
	checkBudgetPanic(t, runRefused(d, func() { allocExact(0, 1, 1<<20) }))
	m[key{n: -1}] = elem{}
	if len(m) != 101 {
		t.Fatalf("map has %d entries, want 101", len(m))
	}
}

// BenchmarkAllocBudget measures an allocation in a domain with a budget
// it does not reach. (math.MaxUint64 would be no budget at all.)
func BenchmarkAllocBudget(b *testing.B) {
	d := goplus.NewAllocDomain()
	d.SetBudget(math.MaxUint64 - 1)
	d.Run(func() {
		for b.Loop() {
			allocSinks[0] = make([]byte, 64)
		}
	})
}
