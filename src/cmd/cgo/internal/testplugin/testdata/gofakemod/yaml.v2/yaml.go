// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package yaml is imported by the escname host AND its plugin. Its
// import path's FINAL element carries a dot, which objabi.PathToPrefix
// escapes: every symbol of this package is spelled
// `go.example.org/fakemod/yaml%2ev2.X`. That is the shape of every
// gopkg.in path in the wild (yaml.v2, yaml.v3, check.v1, mgo.v2), and
// a '%' in the name used to be read as a stdcall `%n` decoration when
// the plugin bound the symbol from the host's import library, failing
// the link (trendvidia/go-archive#116).
package yaml

// DefaultMapType is the host-shared global.
var DefaultMapType int

// Set writes DefaultMapType from inside whichever module calls it.
func Set(n int) {
	DefaultMapType = n
}
