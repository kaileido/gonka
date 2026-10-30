package perf

import "sync"

type inflightGauge[K comparable] struct {
	mu     sync.Mutex
	counts map[K]int
}

func newInflightGauge[K comparable]() *inflightGauge[K] {
	return &inflightGauge[K]{counts: make(map[K]int)}
}

func (g *inflightGauge[K]) acquire(key K) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counts[key]++
}

func (g *inflightGauge[K]) release(key K) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := g.counts[key] - 1; n > 0 {
		g.counts[key] = n
	} else {
		delete(g.counts, key)
	}
}

func (g *inflightGauge[K]) count(key K) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts[key]
}
