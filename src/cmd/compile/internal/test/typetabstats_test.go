// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/buildcfg"
	"internal/testenv"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// typetabStatsRe matches the -d=typetabstats report WriteTypetabSlots
// prints to stderr at dumpdata time.
var typetabStatsRe = regexp.MustCompile(`(?m)^typetabstats: workers=(\d+) typeptr serial=(\d+) parallel=(\d+) itabptr serial=(\d+) parallel=(\d+)$`)

// TestTypetabStatsSerialItabptr pins the concurrency claim in
// reflectdata.itabptrLsym: walk never reaches it while the parallel
// backend phase (base.Ctxt.InParallel) is in effect, because walk runs
// in gc.prepareFunc off the enqueue loop, which only resumes after
// compileFunctions' wg.Wait (trendvidia/go-archive#33, #55).
//
// The pin is a counter, not an assert, so a stale comment fails this
// test on the measured configuration instead of ICEing the compiler on
// an unmeasured GOOS/GOARCH or buildmode. It measures exactly what it
// compiles: one fixture with -c=2 on the host.
//
// Two controls keep the zero from being trivial. The reported worker
// count must exceed 1: gc.compileFunctions sets base.Ctxt.InParallel
// around the compile loop even on the solo-worker path, so a run whose
// -c=2 was silently clamped (base.ParseFlags clamps Flag.LowerC to 1
// whenever concurrentBackendAllowed is false — GOEXPERIMENT=fieldtrack,
// -dynlink, -race, -v, -live, or any -d= flag on this command line
// lacking concurrent:"ok") still reports parallel recordings and would
// otherwise pass green with no worker goroutine ever started. Measured:
// -c=1, -c=2 and -c=8 printed byte-identical phase counts before the
// worker count was added to the report. Second, the typeptr parallel
// count must be non-zero — ssagen.reflectType records typeptr slots
// from backend workers, so a zero there would mean the run never
// observed the parallel phase and the itabptr zero proves nothing.
// Verified to fire (trendvidia/go-archive#55): moving prepareFunc's walk.Walk
// call into compileFunctions' worker closure ICEs before any slot is
// recorded (walk needs serial-only state: staticValues, CalcSize), so
// the check was exercised by setting base.Ctxt.InParallel around the
// walk.Walk call in prepareFunc instead. That took itabptr parallel
// from 0 to 1 (the fixture's other two itabptr recordings come from
// noder, not walk) and failed this test.
func TestTypetabStatsSerialItabptr(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()
	src := filepath.Join("testdata", "reproducible", "typetabslots.go")
	obj := filepath.Join(t.TempDir(), "typetabslots.o")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "compile",
		"-p=typetabslots", "-c=2", "-typetab", "-d=typetabstats=1",
		"-o", obj, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v:\n%s", cmd.Args, err, out)
	}
	m := typetabStatsRe.FindSubmatch(out)
	if m == nil {
		t.Fatalf("%v: no typetabstats report in output:\n%s", cmd.Args, out)
	}
	n := make([]int, 5)
	for i := range n {
		n[i], err = strconv.Atoi(string(m[i+1]))
		if err != nil {
			t.Fatalf("bad count %q in %q: %v", m[i+1], m[0], err)
		}
	}
	workers, typeptrSerial, typeptrParallel, itabptrSerial, itabptrParallel := n[0], n[1], n[2], n[3], n[4]
	t.Logf("%s", m[0])
	if workers < 2 {
		// GOEXPERIMENT=fieldtrack is the only clamp source not under
		// this test's control; there the concurrent backend does not
		// exist, so the claim cannot be exercised rather than being
		// violated. Every other cause is a flag on the command line
		// above, and that is a regression this test must report.
		if buildcfg.Experiment.FieldTrack {
			t.Skipf("backend clamped to %d worker(s) by GOEXPERIMENT=fieldtrack; this pin needs a concurrent backend", workers)
		}
		t.Fatalf("backend ran with %d worker(s) despite -c=2: base.ParseFlags clamped Flag.LowerC because concurrentBackendAllowed() is false, so no worker goroutine started and the itabptr parallel count below would pin nothing; check the flags on this command line (a -d= flag missing concurrent:\"ok\", -race, -dynlink, -v or -live)", workers)
	}
	if typeptrSerial == 0 || itabptrSerial == 0 {
		t.Fatalf("fixture recorded no slots (typeptr serial=%d, itabptr serial=%d); the counters or the fixture regressed", typeptrSerial, itabptrSerial)
	}
	if typeptrParallel == 0 {
		t.Fatalf("no typeptr slot was recorded in the parallel phase; the run did not exercise InParallel, so the itabptr count below pins nothing")
	}
	if itabptrParallel != 0 {
		t.Errorf("itabptrLsym was reached %d time(s) with base.Ctxt.InParallel set; walk now overlaps a backend worker, so the itabptrLsym comment in reflectdata/reflect.go is stale", itabptrParallel)
	}
}
