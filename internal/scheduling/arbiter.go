// Package scheduling owns the transport-independent P0-P4 selection policy.
// Both prototype adapters use this exact arbiter; only their job execution
// differs (HTTP/2 stream send versus WebSocket frame write).
package scheduling

import (
	"context"
	"reflect"
	"sync"

	"github.com/east-true/dockpilot/internal/transport"
)

// The weighted cycle protects P0/P1 while still giving bounded opportunities
// to P2-P4 when protected traffic is continuous.
var weightedCycle = [...]transport.Class{
	transport.ClassControl, transport.ClassControl, transport.ClassControl, transport.ClassControl,
	transport.ClassControl, transport.ClassControl, transport.ClassControl, transport.ClassControl,
	transport.ClassDurable, transport.ClassDurable, transport.ClassDurable, transport.ClassDurable,
	transport.ClassQuery, transport.ClassQuery,
	transport.ClassBulk,
	transport.ClassLive,
}

type Arbiter[T any] struct {
	queues   [transport.NumClasses]chan T
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	next     int
}

func New[T any](queueCapacity int) *Arbiter[T] {
	if queueCapacity <= 0 {
		queueCapacity = 256
	}
	a := &Arbiter[T]{stop: make(chan struct{}), done: make(chan struct{})}
	for i := range a.queues {
		a.queues[i] = make(chan T, queueCapacity)
	}
	return a
}

func (a *Arbiter[T]) Enqueue(ctx context.Context, class transport.Class, job T) error {
	if !class.Valid() {
		return transport.Errorf(transport.CodeProtocol, "invalid traffic class %d", class)
	}
	select {
	case <-a.stop:
		return transport.Errorf(transport.CodeUnavailable, "scheduler stopped")
	case <-ctx.Done():
		return transport.Wrap(transport.StatusOf(ctx.Err()).Code, ctx.Err(), "scheduler enqueue canceled")
	case a.queues[class] <- job:
		return nil
	}
}

// Run selects jobs using the shared weighted policy. handle may perform the
// job synchronously (WebSocket's sole writer) or launch it (gRPC stream sends,
// which must not let one blocked stream hold the arbiter).
func (a *Arbiter[T]) Run(handle func(T) error) error {
	defer close(a.done)
	for {
		job, ok := a.nextJob()
		if !ok {
			return nil
		}
		if err := handle(job); err != nil {
			return err
		}
	}
}

func (a *Arbiter[T]) nextJob() (T, bool) {
	var zero T
	for offset := 0; offset < len(weightedCycle); offset++ {
		idx := (a.next + offset) % len(weightedCycle)
		class := weightedCycle[idx]
		select {
		case job := <-a.queues[class]:
			a.next = (idx + 1) % len(weightedCycle)
			return job, true
		default:
		}
	}
	cases := make([]reflect.SelectCase, 0, len(a.queues)+1)
	for _, queue := range a.queues {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(queue)})
	}
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(a.stop)})
	chosen, value, _ := reflect.Select(cases)
	if chosen == len(a.queues) {
		return zero, false
	}
	class := transport.Class(chosen)
	for i, weighted := range weightedCycle {
		if weighted == class {
			a.next = (i + 1) % len(weightedCycle)
			break
		}
	}
	return value.Interface().(T), true
}

func (a *Arbiter[T]) Stop()                 { a.stopOnce.Do(func() { close(a.stop) }) }
func (a *Arbiter[T]) Done() <-chan struct{} { return a.done }
func (a *Arbiter[T]) Len(class transport.Class) int {
	if !class.Valid() {
		return 0
	}
	return len(a.queues[class])
}
