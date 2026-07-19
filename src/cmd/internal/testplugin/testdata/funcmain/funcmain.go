// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Plugin whose main package declares `func main`. The fork's
// -buildmode=plugin must accept-and-ignore it (trendvidia/go#6) so a
// single package can satisfy both `go build ./...` (stock default
// mode, which requires func main to link) and a real plugin build,
// without a per-plugin build-tag stub. The exported symbol lets a
// host confirm the plugin still loads with func main present.
package main

import "testplugin/common"

// FuncMainMarker is looked up by the host to confirm the plugin loaded
// even though the main package declares func main.
func FuncMainMarker() int { return common.X }

func main() {}
