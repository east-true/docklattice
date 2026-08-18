package producttransport

import (
	"context"
	"fmt"
	"sync"
)

type TrafficClass uint8

const (
	P0Control TrafficClass = iota
	P1DurableSync
	P2InteractiveQuery
	P3BulkInteractive
	P4DisposableLive
)

func (c TrafficClass) valid() bool { return c <= P4DisposableLive }

// PriorityGate reserves independent bounded concurrency for P0, P1, and bulk
// traffic. Slow control calls cannot stop durable Audit progress, and saturated
// P2-P4 work cannot consume either protected pool.
type PriorityGate struct {
	control     chan struct{}
	durableSync chan struct{}
	bulk        chan struct{}
}

func NewPriorityGate(protectedConcurrency, bulkConcurrency int) (*PriorityGate, error) {
	if protectedConcurrency <= 0 || bulkConcurrency <= 0 {
		return nil, fmt.Errorf("priority gate concurrency must be positive")
	}
	return &PriorityGate{
		control:     make(chan struct{}, protectedConcurrency),
		durableSync: make(chan struct{}, protectedConcurrency),
		bulk:        make(chan struct{}, bulkConcurrency),
	}, nil
}

func (g *PriorityGate) Acquire(ctx context.Context, class TrafficClass) (func(), error) {
	if !class.valid() {
		return nil, fmt.Errorf("invalid traffic class %d", class)
	}
	slots := g.bulk
	switch class {
	case P0Control:
		slots = g.control
	case P1DurableSync:
		slots = g.durableSync
	}
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
