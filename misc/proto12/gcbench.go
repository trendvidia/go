//go:build ignore

// gcbench measures full-collection time on a heap of objects that carry
// a type header (1KB, pointerful) or a large-object type (64KB), the
// objects for which trendvidia/go#12's prototype adds work to the scan.
package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"
)

type hdr struct {
	p   *int
	pad [1016]byte
}

type big struct {
	p   *int
	pad [65528]byte
}

var keepH []*hdr
var keepB []*big

func main() {
	x := new(int)
	switch os.Args[1] {
	case "hdr":
		for range 400_000 {
			keepH = append(keepH, &hdr{p: x})
		}
	case "big":
		for range 6_000 {
			keepB = append(keepB, &big{p: x})
		}
	}
	runtime.GC()
	var d []time.Duration
	for range 30 {
		t := time.Now()
		runtime.GC()
		d = append(d, time.Since(t))
	}
	slices.Sort(d)
	fmt.Printf("%s median=%v min=%v\n", os.Args[1], d[len(d)/2], d[0])
}
