// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objabi

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"strings"
	"testing"
)

// go-plus (trendvidia/go#20): -V=full reports the tool's build ID for a
// release as well as for a development version, so that cmd/go keys the
// build cache on the tool's content. A tool with no build ID (one linked
// externally) reports its version line alone, as upstream does.
func TestVersionFlagBuildID(t *testing.T) {
	if v, ok := os.LookupEnv("OBJABI_TEST_VERSIONFLAG"); ok {
		buildcfg.Version, buildID, _ = strings.Cut(v, "|")
		versionFlag{}.Set("full") // exits
		t.Fatal("versionFlag.Set returned")
	}

	const devel = "devel go1.28-0123456789 Fri Oct 16 00:00:00 2026 +0000"
	tests := []struct {
		version, buildID string
		want             string // the line's buildID= field, or "" for none
	}{
		{"go1.27.2", "aaaa/cccc", "buildID=aaaa/cccc"},
		{"go1.27.2", "", ""},
		{devel, "aaaa/cccc", "buildID=aaaa/cccc"},
		{devel, "", "buildID="},
	}
	for _, tt := range tests {
		cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestVersionFlagBuildID$")
		cmd.Env = append(os.Environ(), "OBJABI_TEST_VERSIONFLAG="+tt.version+"|"+tt.buildID)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("version %q, buildID %q: %v\n%s", tt.version, tt.buildID, err, out)
		}
		line := strings.TrimSpace(string(out))
		f := strings.Fields(line)
		got := ""
		if id := f[len(f)-1]; strings.HasPrefix(id, "buildID=") {
			got = id
		}
		if !strings.HasPrefix(line, f[0]+" version "+tt.version) || got != tt.want || strings.Count(line, "buildID=") > 1 {
			t.Errorf("version %q, buildID %q: -V=full printed %q, want build ID field %q", tt.version, tt.buildID, line, tt.want)
		}
	}
}
