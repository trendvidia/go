//go:build !windows

package main

type memSample struct {
	reserved uint64
	private  uint64
}

// Not the platform under test; the rehearsal targets Windows, where
// unload leaves a reservation behind.
func sampleMem() memSample { return memSample{} }
