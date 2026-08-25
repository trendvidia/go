// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command samename pins trendvidia/go#40's done-when: two DIFFERENT
// plugins installed under one base file name (same-a/p.ext and
// same-b/p.ext) must never collapse onto one OS module. Depending on
// the platform loader's matching rule the second Open either loads B
// as its own module or is rejected with the named base-name error;
// both outcomes are accepted. The forbidden third outcome — success
// while answering A — is the silent alias the fence exists to prevent,
// and fails here.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"plugin"
	"runtime"
	"strings"
)

var pluginExt = func() string {
	if runtime.GOOS == "windows" {
		return ".dll"
	}
	return ".so"
}()

func name(p *plugin.Plugin) string {
	f, err := p.Lookup("Name")
	if err != nil {
		log.Fatalf(`Lookup("Name") failed: %v`, err)
	}
	return f.(func() string)()
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	paPath := filepath.Join(cwd, "same-a", "p"+pluginExt)
	pbPath := filepath.Join(cwd, "same-b", "p"+pluginExt)

	pa, err := plugin.Open(paPath)
	if err != nil {
		log.Fatalf("plugin.Open(%q) failed: %v", paPath, err)
	}
	if got, want := name(pa), "A"; got != want {
		log.Fatalf("%s: Name()=%q, want %q", paPath, got, want)
	}

	pb, err := plugin.Open(pbPath)
	switch {
	case err != nil:
		// Rejected — but only the named base-name rejection counts;
		// any other failure mode is its own bug.
		const named = "already loaded from"
		if !strings.Contains(err.Error(), named) {
			log.Fatalf("plugin.Open(%q) = %v; want the %q rejection", pbPath, err, named)
		}
		fmt.Printf("second plugin rejected with named error: %v\n", err)
	default:
		if got, want := name(pb), "B"; got != want {
			log.Fatalf("plugin.Open(%q) silently aliased %q: Name()=%q, want %q",
				pbPath, paPath, got, want)
		}
		fmt.Println("second plugin loaded as its own module")
	}

	// Either outcome must leave the first plugin intact and still
	// answering for itself.
	if got, want := name(pa), "A"; got != want {
		log.Fatalf("%s: Name()=%q after second open, want %q", paPath, got, want)
	}
	fmt.Println("PASS")
}
