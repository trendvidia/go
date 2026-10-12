// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A plugin whose only cgo is the import itself: enough to put
// runtime/cgo in the link, not enough to pull in libcgo's C half.
// Nothing here references the runtime-init handshake, so deadcode drops
// it and the image carries neither _cgo_wait_runtime_init_done to block
// on nor x_cgo_notify_runtime_init_done to publish.
//
// This is the shape that broke when cmd/link exported the notify from
// every cgo plugin (trendvidia/go-archive#130): on the internal-link path an
// unreachable symbol has no address, so the export published a
// placeholder and plugin.Open called into the PE headers.
package main

import "C"

func F() int { return 7 }

func main() {}
