// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64

package simd_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"regexp"
	"runtime"
	"simd"
	"simd/archsimd"
	"strings"
	"testing"
)

// TestReportMode is the child half of TestAVXOnlyEmulates: it logs the mode
// this package chose at init so the parent can read it back through -test.v.
// It passes trivially when run by itself.
func TestReportMode(t *testing.T) {
	t.Logf("simdmode emulated=%v size=%d", simd.Emulated(), simd.VectorBitSize())
}

var modeRE = regexp.MustCompile(`simdmode emulated=(true|false) size=(\d+)`)

// TestAVXOnlyEmulates pins that a CPU with AVX but without AVX2 and FMA gets
// emulated SIMD, not hardware SIMD at 128 bits. The bridge's 128-bit
// Broadcast* go through VPBROADCAST (AVX2) and MulAdd through VFMADD (FMA), so
// hardware SIMD on such a CPU is a SIGILL in TestMulAdd and TestBroadcast --
// measured on an Ivy Bridge i7-3720QM, trendvidia/go#154.
//
// The CPU is shaped by GODEBUG cpu.* masks in a child process, so the pin
// fails on every amd64 host that lacks the check, not only on the hardware
// that would crash. The masks are inert on Windows (the runtime reads them
// from the environment only on the Unix-like GOOSes), hence the skip there.
// The child also runs the in-process tests that SIGILL without the fix, which
// proves the emulated path computes the right answers rather than merely not
// crashing. (TestIFace crashed too, but it shells out to the go tool, so it
// stays out of the child.)
func TestAVXOnlyEmulates(t *testing.T) {
	testenv.MustHaveExec(t)
	if runtime.GOOS == "windows" {
		t.Skip("GODEBUG cpu.* options are not read on windows")
	}
	if !archsimd.X86.AVX() {
		t.Skip("no AVX: nothing to mask down to")
	}
	// No testing.Short() skip on purpose: cmd/dist runs every package test
	// with -short, and a short-skipped pin guards nothing in all.bash.

	// Ivy Bridge shape: AVX, no AVX2, no FMA. avx512f=off is what clears
	// the derived HasAVX512, which the cpu.* options do not cascade to.
	const ivyBridge = "cpu.avx2=off,cpu.fma=off,cpu.avx512f=off"

	cases := []struct {
		name     string
		godebug  string
		emulated bool
		size     int
		// tests the child runs besides TestReportMode; must all pass
		run []string
	}{
		{
			name:     "avx-only",
			godebug:  ivyBridge,
			emulated: true,
			size:     128,
			run:      []string{"TestMulAdd", "TestBroadcast", "TestMaskToInt"},
		},
		{
			// AVX2 without FMA does not ship, but the check is the same
			// conjunction and this is the leg that proves FMA is in it.
			name:     "no-fma",
			godebug:  "cpu.fma=off,cpu.avx512f=off",
			emulated: true,
			size:     128,
			run:      []string{"TestMulAdd"},
		},
		{
			// The documented override: "+N" keeps hardware SIMD at N bits
			// even though features are missing. Only the report runs here;
			// on real AVX-only hardware the other tests would SIGILL, which
			// is exactly what the override promises.
			name:     "avx-only-override",
			godebug:  ivyBridge + ",simd=+128",
			emulated: false,
			size:     128,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := append([]string{"TestReportMode"}, tc.run...)
			pattern := "^(" + strings.Join(names, "|") + ")$"
			cmd := testenv.Command(t, os.Args[0], "-test.run="+pattern, "-test.v", "-test.count=1")
			godebug := tc.godebug
			if have := os.Getenv("GODEBUG"); have != "" {
				godebug = have + "," + godebug
			}
			cmd.Env = append(os.Environ(), "GODEBUG="+godebug)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child failed: %v\n%s", err, out)
			}
			for _, name := range names {
				if !strings.Contains(string(out), "--- PASS: "+name+" ") {
					t.Errorf("no PASS line for %s in child output:\n%s", name, out)
				}
			}
			m := modeRE.FindStringSubmatch(string(out))
			if m == nil {
				t.Fatalf("child did not report its mode:\n%s", out)
			}
			got := "emulated=" + m[1] + " size=" + m[2]
			want := fmt.Sprintf("emulated=%v size=%d", tc.emulated, tc.size)
			if got != want {
				t.Errorf("GODEBUG=%s: got %s, want %s", tc.godebug, got, want)
			}
		})
	}
}
