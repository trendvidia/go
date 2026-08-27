// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/testenv"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// typetabSlotsWant is every slot testdata/reproducible/typetabslots.go
// produces under -typetab, with the compiler path that records it.
//
// Naming the slots matters: asserting only that the object holds *some*
// go.typeptr. symbol does not pin TypePtrForUseAt, because the two
// named-type slots are recorded by ssagen.reflectType as well. Measured
// on this fixture: with TypePtrForUseAt's -typetab branch forced off,
// those two slots are still emitted and only go.typeptr.type:interface {}
// disappears, so a non-empty check stays green with that path dead.
var typetabSlotsWant = []string{
	"go.itabptr.*typetabslots.Counter,typetabslots.Increments", // ITabAddrForUseAt
	"go.typeptr.type:*typetabslots.Counter",                    // TypePtrForUseAt, also ssagen.reflectType
	"go.typeptr.type:interface {}",                             // TypePtrForUseAt only
	"go.typeptr.type:typetabslots.Increments",                  // TypePtrForUseAt, also ssagen.reflectType
}

// TestTypetabSlotsPresent pins that compiling with -typetab routes
// interface construction through per-module go.typeptr./go.itabptr.
// slots, by asserting each slot above is emitted as writable data.
// The flag is force-enabled only for Windows plugin-compatible builds
// (cmd/go/internal/work/init.go), so without this check the slot paths
// in TypePtrForUseAt/ITabAddrForUseAt run on no non-Windows CI leg
// (trendvidia/go#34). The default-flags run is the control proving the
// assertion machinery notices absent slots.
func TestTypetabSlotsPresent(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()
	src := filepath.Join("testdata", "reproducible", "typetabslots.go")
	for _, tc := range []struct {
		name    string
		typetab bool
	}{
		{name: "typetab", typetab: true},
		{name: "default", typetab: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := filepath.Join(t.TempDir(), "typetabslots.o")
			args := []string{"tool", "compile", "-p=typetabslots"}
			if tc.typetab {
				args = append(args, "-typetab")
			}
			args = append(args, "-o", obj, src)
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %v:\n%s", cmd.Args, err, out)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", obj)
			out, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %v:\n%s", nm.Args, err, out)
			}
			got := typetabSlotSyms(string(out))
			if !tc.typetab {
				if len(got) > 0 {
					t.Errorf("object compiled without -typetab has slot symbols %q; slots must be flag-gated", slices.Sorted(maps.Keys(got)))
				}
				return
			}
			for _, want := range typetabSlotsWant {
				code, ok := got[want]
				switch {
				case !ok:
					t.Errorf("-typetab object has no %s symbol; slot emission regressed\nnm output:\n%s", want, out)
				case code != "D":
					// staticdata.SlotRegistry.Write forces slots to
					// SNOPTRDATA so they stay writable for the runtime's
					// module-load rewrite; anything else ('U' for a slot
					// recorded but never emitted, 'R' for one that landed
					// read-only) breaks that rewrite.
					t.Errorf("-typetab object has %s as nm code %s, want D (writable data)", want, code)
				}
			}
		})
	}
}

// nmSymLine matches one go tool nm output line, capturing the symbol
// code and name. nm prints "%8x %c %s" (cmd/nm/nm.go), leaving the
// address column blank for undefined symbols; the name runs to the end
// of the line and contains spaces for many types
// ("go.typeptr.type:interface {}"), so splitting the line on whitespace
// loses exactly those symbols.
var nmSymLine = regexp.MustCompile(`^ *[0-9a-f]* ([A-Za-z?]) (.*)$`)

// typetabSlotSyms returns the -typetab slot symbols named in go tool nm
// output, mapped to their nm code. Undefined ('U') references are kept
// rather than skipped: a slot that is referenced but never emitted is
// the regression this test exists to catch, and it should report the
// code it found rather than silently reading as absent.
func typetabSlotSyms(nmOut string) map[string]string {
	slots := make(map[string]string)
	for _, line := range strings.Split(nmOut, "\n") {
		m := nmSymLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		code, name := m[1], m[2]
		if strings.HasPrefix(name, "go.typeptr.") || strings.HasPrefix(name, "go.itabptr.") {
			slots[name] = code
		}
	}
	return slots
}
