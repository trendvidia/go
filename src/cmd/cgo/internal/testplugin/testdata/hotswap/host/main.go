// A host that upgrades a plugin in place, repeatedly, while live
// traffic keeps flowing through it — the hot-deployment shape, where a
// long-running process swaps a plugin for a newer build without
// restarting.
//
// It is both a pin and a reference. As a pin it holds the two facts an
// upgrade depends on: that the close-before-load order works, and that
// the reverse order does not. As a reference it is the supervisor
// itself — every rule it follows is one plugin.Close's upgrade contract
// states but cannot enforce, and each is commented where it is obeyed.
//
// Run it directly for numbers rather than assertions:
//
//	host -cycles=200 -workers=4
//
// which reports the swap window, the quiesce drain, and the address
// space each load/unload cycle costs.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"plugin"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"testplugin/hotswap/api"
)

// loaded is one generation of the strategy.
//
// The host holds exactly two things from the plugin: func values from
// Lookup. It holds no pointer into the image, no plugin-defined type,
// and no interface whose dynamic type lives in the image -- those are
// what Close cannot protect, and what would dangle the moment the image
// is unmapped.
type loaded struct {
	p       *plugin.Plugin
	onTick  api.OnTick
	version string
	path    string

	// inflight counts calls currently inside onTick. retired is set
	// before draining. Together they are the quiesce protocol: no call
	// may be inside the image when Close runs.
	inflight atomic.Int64
	retired  atomic.Bool
}

var (
	active   atomic.Pointer[loaded]
	ticksOK  atomic.Int64
	staleHit atomic.Int64 // outputs from a generation already retired
	deferred atomic.Int64 // ticks arriving while no strategy was serving
	stopping atomic.Bool
)

func loadGeneration(srcDLL, dstPath string) (*loaded, error) {
	if err := copyFile(srcDLL, dstPath); err != nil {
		return nil, err
	}
	p, err := plugin.Open(dstPath)
	if err != nil {
		return nil, fmt.Errorf("Open(%s): %w", dstPath, err)
	}
	fn, err := p.Lookup("OnTick")
	if err != nil {
		return nil, fmt.Errorf("Lookup OnTick: %w", err)
	}
	// The assertion is to a HOST-defined signature over HOST-defined
	// types. If cross-module type identity were broken this is where it
	// would fail, loudly, rather than corrupting later.
	onTick, ok := fn.(func(api.Tick) api.Order)
	if !ok {
		return nil, fmt.Errorf("OnTick has type %T, want func(api.Tick) api.Order", fn)
	}
	vfn, err := p.Lookup("Version")
	if err != nil {
		return nil, fmt.Errorf("Lookup Version: %w", err)
	}
	vf, ok := vfn.(func() string)
	if !ok {
		return nil, fmt.Errorf("Version has type %T", vfn)
	}
	return &loaded{p: p, onTick: onTick, version: vf(), path: dstPath}, nil
}

// worker is the hot path. It runs for the whole test and never restarts
// across an upgrade -- that is the point: the traffic does not stop.
func worker(id int, lat *latencyRec) {
	var seq int64
	for !stopping.Load() {
		seq++
		l := active.Load()
		if l == nil || l.retired.Load() {
			// No strategy is serving: the swap window. A real system
			// queues here; the rehearsal counts what it would queue.
			deferred.Add(1)
			runtime.Gosched()
			continue
		}
		l.inflight.Add(1)
		// Re-check AFTER incrementing. A generation retired between the
		// Load and the Add must not be entered: the increment is what
		// makes the drain wait for us, so the check has to come after
		// it or the drain can miss an in-flight call.
		if l.retired.Load() {
			l.inflight.Add(-1)
			deferred.Add(1)
			continue
		}
		t := api.Tick{Sym: "ES", Px: 100 + float64(seq%7), Seq: seq}
		start := time.Now()
		o := l.onTick(t)
		lat.add(time.Since(start))
		l.inflight.Add(-1)

		if o.Note != l.version {
			staleHit.Add(1)
		}
		ticksOK.Add(1)
	}
}

// upgrade replaces the running strategy.
//
// The order is forced, and it is the central finding of this rehearsal:
// the OLD generation must be closed BEFORE the new one is loaded.
//
// Loading first and closing second -- the obvious pattern, and the one
// tried first -- does not work. runtime.recordPkgDependencies registers
// the first module to contain a given (package, link-time hash) as its
// owner, and every later module carrying that package records a
// dependency on the owner. Two strategy builds share their SDK package,
// so generation N+1 depends on generation N, and Close(N) refuses:
//
//	plugin has dependents; Close them first: rehearsal/strat_v2
//
// Since the dependency always points from newer to older, only the
// newest generation is ever closable, which is the opposite of what an
// upgrade needs. Closing first breaks the overlap and so breaks the
// dependency -- at the cost of a window in which no strategy is
// serving, which is what this measures.
func upgrade(cur *loaded, srcDLL, dstPath string) (*loaded, time.Duration, time.Duration) {
	// 1. Close the door and let in-flight calls finish. Nothing may be
	//    inside the image when Close runs.
	cur.retired.Store(true)
	drainStart := time.Now()
	for cur.inflight.Load() > 0 {
		runtime.Gosched()
	}
	drain := time.Since(drainStart)

	// 2. The window: old strategy gone, new one not yet up.
	windowStart := time.Now()
	if err := cur.p.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Close(%s): %v  dependents=%v\n", cur.path, err, cur.p.Dependents())
		os.Exit(1)
	}
	os.Remove(cur.path)

	next, err := loadGeneration(srcDLL, dstPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load replacement:", err)
		os.Exit(1)
	}
	active.Store(next)
	return next, drain, time.Since(windowStart)
}

func main() {
	probeDep := flag.Bool("probe-dependency", false, "load two generations and try to close the older one")
	v1 := flag.String("v1", "strat_v1.so", "first strategy build")
	v2 := flag.String("v2", "strat_v2.so", "second strategy build")
	cycles := flag.Int("cycles", 20, "upgrade cycles")
	workers := flag.Int("workers", 4, "hot-path goroutines")
	settle := flag.Duration("settle", 25*time.Millisecond, "traffic time between upgrades")
	flag.Parse()

	if *probeDep {
		// Load-then-close: the order an upgrade would naturally use.
		a, err := loadGeneration(*v1, "probe_a"+ext(*v1))
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe load a:", err)
			os.Exit(1)
		}
		b, err := loadGeneration(*v2, "probe_b"+ext(*v2))
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe load b:", err)
			os.Exit(1)
		}
		_ = b
		fmt.Printf("PROBE older.HasDependents=%v dependents=%v\n", a.p.HasDependents(), a.p.Dependents())
		fmt.Printf("PROBE Close(older) err=%v\n", a.p.Close())
		return
	}

	lat := newLatencyRec()
	gen, err := loadGeneration(*v1, "gen_0"+ext(*v1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "initial load:", err)
		os.Exit(1)
	}
	active.Store(gen)

	for i := 0; i < *workers; i++ {
		go worker(i, lat)
	}

	memStart := sampleMem()
	drains := make([]time.Duration, 0, *cycles)
	windows := make([]time.Duration, 0, *cycles)
	t0 := time.Now()

	for c := 1; c <= *cycles; c++ {
		time.Sleep(*settle)
		src := *v2
		if c%2 == 0 {
			src = *v1
		}
		cur := active.Load()
		_, drain, window := upgrade(cur, src, fmt.Sprintf("gen_%d%s", c, ext(src)))
		drains = append(drains, drain)
		windows = append(windows, window)
		if c%50 == 0 {
			m := sampleMem()
			fmt.Printf("  cycle %3d  ticks=%-9d reservedMB=%-7.2f privateMB=%-7.2f\n",
				c, ticksOK.Load(), float64(m.reserved)/(1<<20), float64(m.private)/(1<<20))
		}
	}
	elapsed := time.Since(t0)
	stopping.Store(true)
	time.Sleep(50 * time.Millisecond)
	memEnd := sampleMem()

	sort.Slice(drains, func(i, j int) bool { return drains[i] < drains[j] })
	fmt.Println()
	fmt.Println("=== gameflow hot-deployment rehearsal ===")
	fmt.Printf("cycles                 %d over %v\n", *cycles, elapsed.Round(time.Millisecond))
	fmt.Printf("workers                %d\n", *workers)
	fmt.Printf("ticks through strategy %d\n", ticksOK.Load())
	fmt.Printf("stale-generation calls %d   (must be 0)\n", staleHit.Load())
	sort.Slice(windows, func(i, j int) bool { return windows[i] < windows[j] })
	fmt.Printf("ticks deferred (swap window) %d\n", deferred.Load())
	fmt.Printf("swap window p50/p99/max %v / %v / %v   <- no strategy serving\n",
		windows[len(windows)/2].Round(time.Microsecond),
		windows[len(windows)*99/100].Round(time.Microsecond),
		windows[len(windows)-1].Round(time.Microsecond))
	fmt.Printf("drain p50 / p99 / max  %v / %v / %v\n",
		drains[len(drains)/2].Round(time.Microsecond),
		drains[len(drains)*99/100].Round(time.Microsecond),
		drains[len(drains)-1].Round(time.Microsecond))
	fmt.Printf("OnTick p50 / p99 / max %v\n", lat.summary())
	fmt.Printf("reserved VA  %7.2f MB -> %7.2f MB   (%+.1f KiB per cycle)\n",
		float64(memStart.reserved)/(1<<20), float64(memEnd.reserved)/(1<<20),
		float64(memEnd.reserved-memStart.reserved)/1024/float64(*cycles))
	fmt.Printf("private      %7.2f MB -> %7.2f MB   (%+.1f KiB per cycle)\n",
		float64(memStart.private)/(1<<20), float64(memEnd.private)/(1<<20),
		float64(memEnd.private-memStart.private)/1024/float64(*cycles))
}

// ext returns path's extension, so generation files match whatever the
// platform's plugins are called.
func ext(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i:]
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
