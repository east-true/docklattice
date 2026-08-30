package wsadapter

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/east-true/docklattice/internal/scheduling"
	"github.com/east-true/docklattice/internal/transport"
)

// scheduler owns the only connection writer, while scheduling.Arbiter owns the
// candidate-neutral P0-P4 selection policy.
type scheduler struct {
	conn        net.Conn
	arbiter     *scheduling.Arbiter[frame]
	done        <-chan struct{}
	err         chan error
	queuedBytes atomic.Int64
}

func newScheduler(conn net.Conn) *scheduler {
	arbiter := scheduling.New[frame](256)
	s := &scheduler{conn: conn, arbiter: arbiter, done: arbiter.Done(), err: make(chan error, 1)}
	go func() {
		if err := arbiter.Run(func(f frame) error {
			err := writeFrame(conn, f)
			s.queuedBytes.Add(-int64(wireHeaderBytes + len(f.payload)))
			return err
		}); err != nil {
			s.err <- err
		}
	}()
	return s
}

func (s *scheduler) enqueue(ctx context.Context, f frame) error {
	size := int64(wireHeaderBytes + len(f.payload))
	s.queuedBytes.Add(size)
	if err := s.arbiter.Enqueue(ctx, f.class, f); err != nil {
		s.queuedBytes.Add(-size)
		return err
	}
	return nil
}

func (s *scheduler) close() { s.arbiter.Stop() }

func (s *scheduler) queueLen(class transport.Class) int { return s.arbiter.Len(class) }
func (s *scheduler) queueBytes() int64                  { return s.queuedBytes.Load() }
