// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goplus

import "internal/goversion"

// Release returns the go-plus release the toolchain that built this
// program is built from, such as "v1.27.3b". A toolchain built between
// releases reports the last release; [Revision] tells such builds apart.
//
// The go command also records it in every binary as the build setting
// "goplus", which a program reads through [runtime/debug.ReadBuildInfo]
// without importing this package.
func Release() string { return goversion.PlusRelease }

// Revision returns the Git commit the toolchain that built this program
// is built from: the full commit hash, followed by "+dirty" when the
// toolchain was built from a checkout whose tracked files differed from
// that commit. It is "" when the toolchain was built from a source
// archive rather than a Git checkout.
//
// The go command also records a non-empty revision in every binary as
// the build setting "goplus.revision".
func Revision() string { return goversion.PlusRevision }
