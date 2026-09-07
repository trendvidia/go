// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import yaml "go.example.org/fakemod/yaml.v2"

// SetIt writes the host-shared yaml.DefaultMapType.
func SetIt(n int) {
	yaml.Set(n)
}

// GetIt reads it back.
func GetIt() int {
	return yaml.DefaultMapType
}

func main() {}
