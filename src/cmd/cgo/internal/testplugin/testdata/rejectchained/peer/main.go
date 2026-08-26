// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// peer is the healthy plugin the rejectchained host opens BEFORE it
// loads the patched victim, so the rejection happens on a chain whose
// tail is another plugin's moduledata rather than the host's. A
// distinct package (and therefore a distinct pluginpath) from victim,
// so both can be open in the same process at once.
package main

// Name identifies this plugin.
func Name() string { return "P" }
