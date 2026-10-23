// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// A recursion through cgo callbacks inside goplus.AllocDomain.Run: Go
// calls C, which calls back Go, which calls C again. Each growth of the
// goroutine's stack comes at the entry of the callback, whose caller is
// the runtime's cgocallbackg1, so it is refused only by the rule for
// ordinary code the runtime calls back (trendvidia/go#100); without it,
// the recursion runs on to the process-wide maximum.

/*
void goplusMaxStackCall(int);
*/
import "C"

import (
	"errors"
	"fmt"
	"goplus"
	"runtime/debug"
)

func init() {
	register("GoplusMaxStackCgo", GoplusMaxStackCgo)
}

// goplusCgoFrame is about the stack each level of the recursion uses.
const goplusCgoFrame = 16 << 10

var (
	goplusCgoDeepest int
	goplusCgoSink    byte
)

//export goplusMaxStackBack
func goplusMaxStackBack(n C.int) {
	var buf [goplusCgoFrame - 64]byte
	buf[int(n)%len(buf)] = byte(n)
	goplusCgoDeepest = int(n)
	C.goplusMaxStackCall(n + 1)
	goplusCgoSink = buf[int(n+1)%len(buf)]
}

func GoplusMaxStackCgo() {
	const limit = 1 << 20
	debug.SetMaxStack(64 * limit)
	d := goplus.NewAllocDomain()
	d.SetMaxStack(limit)
	// Twice on one goroutine: the panic unwinds through the C frames,
	// and the goroutine must be able to call C and be refused again.
	for i := range 2 {
		goplusCgoDeepest = 0
		var p any
		func() {
			defer func() { p = recover() }()
			d.Run(func() { C.goplusMaxStackCall(0) })
		}()
		if err, ok := p.(error); !ok || !errors.Is(err, goplus.ErrMaxStack) {
			fmt.Printf("run %d: Run panicked with %v (%T), want goplus.ErrMaxStack\n", i, p, p)
			return
		}
		// A growth inside the runtime's own callback code is let through,
		// so the stack may double past the limit once before the next
		// growth, at a callback's entry, is refused.
		if used := goplusCgoDeepest * goplusCgoFrame; used < limit/2 || used > 2*limit {
			fmt.Printf("run %d: recursion stopped at %d levels, about %d bytes of stack; want about %d to %d\n", i, goplusCgoDeepest, used, limit/2, 2*limit)
			return
		}
	}
	fmt.Println("OK")
}
