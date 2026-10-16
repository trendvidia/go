// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objabi

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// go-plus (trendvidia/go#20): -V=full reports the tool's build ID for a
// release as well as for a development version, so that cmd/go keys the
// build cache on the tool's content. A tool with no build ID (one linked
// externally) reports its version line alone, as upstream does.
func TestVersionFlagBuildID(t *testing.T) {
	if v, ok := os.LookupEnv("OBJABI_TEST_VERSIONFLAG"); ok {
		f := strings.Split(v, "|")
		buildcfg.Version, buildID = f[0], f[1]
		if f[2] != "" {
			exp, err := buildcfg.ParseGOEXPERIMENT(buildcfg.GOOS, buildcfg.GOARCH, f[2])
			if err != nil {
				t.Fatal(err)
			}
			buildcfg.Experiment = *exp
		}
		versionFlag{}.Set("full") // exits
		t.Fatal("versionFlag.Set returned")
	}

	exe := testenv.Executable(t)
	name := strings.TrimSuffix(filepath.Base(exe), ".exe")
	const devel = "devel go1.28-0123456789 Fri Oct 16 00:00:00 2026 +0000"
	tests := []struct {
		version, buildID, goexperiment string
		want                           string // the line's buildID= field, or "" for none
	}{
		{"go1.27.2", "aaaa/cccc", "", "buildID=aaaa/cccc"},
		{"go1.27.2", "", "", ""},
		{"go1.27.2", "aaaa/cccc", "fieldtrack", "buildID=aaaa/cccc"},
		{devel, "aaaa/cccc", "", "buildID=aaaa/cccc"},
		{devel, "", "", "buildID="},
	}
	for _, tt := range tests {
		// The child keeps this process's experiments unless the case sets its own.
		exp := &buildcfg.Experiment
		if tt.goexperiment != "" {
			var err error
			if exp, err = buildcfg.ParseGOEXPERIMENT(buildcfg.GOOS, buildcfg.GOARCH, tt.goexperiment); err != nil {
				t.Fatal(err)
			}
		}
		want := name + " version " + tt.version
		if x := exp.String(); x != "" {
			want += " X:" + x
		}
		if tt.want != "" {
			want += " " + tt.want
		}

		cmd := testenv.Command(t, exe, "-test.run=^TestVersionFlagBuildID$")
		cmd.Env = append(os.Environ(), "OBJABI_TEST_VERSIONFLAG="+tt.version+"|"+tt.buildID+"|"+tt.goexperiment)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("version %q, buildID %q: %v\n%s", tt.version, tt.buildID, err, out)
		}
		if line := strings.TrimSpace(string(out)); line != want {
			t.Errorf("version %q, buildID %q, GOEXPERIMENT %q: -V=full printed %q, want %q", tt.version, tt.buildID, tt.goexperiment, line, want)
		}
	}
}
