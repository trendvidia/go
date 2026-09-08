package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// latencyRec samples OnTick durations. Sampled rather than exhaustive so
// the recorder does not become the thing being measured.
type latencyRec struct {
	mu sync.Mutex
	d  []time.Duration
	n  int64
}

func newLatencyRec() *latencyRec { return &latencyRec{d: make([]time.Duration, 0, 200000)} }

func (l *latencyRec) add(x time.Duration) {
	l.mu.Lock()
	l.n++
	if l.n%16 == 0 && len(l.d) < cap(l.d) {
		l.d = append(l.d, x)
	}
	l.mu.Unlock()
}

func (l *latencyRec) summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return "no samples"
	}
	sort.Slice(l.d, func(i, j int) bool { return l.d[i] < l.d[j] })
	return fmt.Sprintf("%v / %v / %v (n=%d sampled)",
		l.d[len(l.d)/2], l.d[len(l.d)*99/100], l.d[len(l.d)-1], len(l.d))
}
