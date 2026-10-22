// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goversion

// Version is the Go 1.x version which is currently
// in development and will eventually get released.
//
// It should be updated at the start of each development cycle to be
// the version of the next Go 1.x release. See go.dev/issue/40705.
const Version = 27

// PlusRelease names the go-plus release this toolchain is built from
// (github.com/trendvidia/go). It is the single source for both places
// the release appears: the "(plus …)" marker `go version` prints, and
// the "goplus" build setting the go command records in every binary it
// builds, which a program reads through [runtime/debug.ReadBuildInfo]
// and `go version -m` prints.
//
// PlusRevision, the Git commit the toolchain is built from, is written
// to zplus.go by cmd/dist (mkzgoplus).
//
// Bump it on every go-plus release, before tagging. It is not part of
// [Version] or VERSION because a release letter (v1.27.3b) does not fit
// Go's version grammar: "go1.27.3b" parses as malformed and compares
// below plain "go1.27.3".
const PlusRelease = "v1.27.3c"
