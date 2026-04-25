// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (!linux && !freebsd && !darwin) || !cgo

package plugin

import "errors"

func lookup(p *Plugin, symName string) (Symbol, error) {
	return nil, errors.New("plugin: not implemented")
}

func open(name string) (*Plugin, error) {
	return nil, errors.New("plugin: not implemented")
}

// unloadPlugin is the stub-platform counterpart of the dlclose-based
// phase 2 in plugin_dlopen.go. There is no OS handle to release on
// platforms that do not support plugins, so this is unconditionally
// a no-op. closePlugin still calls it so the platform-independent
// path stays uniform.
func unloadPlugin(p *Plugin) error {
	return nil
}
