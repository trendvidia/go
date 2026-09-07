// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	ext "extensions"
	"flag"
	"reflect"
)

// BumpA bumps ext.Counter by 1 and returns it.
func BumpA() int { return ext.Bump(1) }

// FlagsPtr reports which flag.CommandLine this plugin sees. flag is a
// slashless STDLIB package the host does not link, so before #115 each
// plugin kept its own copy of that variable.
func FlagsPtr() uintptr { return reflect.ValueOf(flag.CommandLine).Pointer() }

func main() {}
