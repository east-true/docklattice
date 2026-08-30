package grpcadapter

import (
	"context"

	"github.com/east-true/docklattice/internal/scheduling"
	"github.com/east-true/docklattice/internal/transport"
)

type sendJob struct {
	start  func() error
	result chan error
}

type sendDispatcher struct {
	arbiter *scheduling.Arbiter[sendJob]
}

func newSendDispatcher() *sendDispatcher {
	d := &sendDispatcher{arbiter: scheduling.New[sendJob](256)}
	go func() {
		_ = d.arbiter.Run(func(job sendJob) error {
			// A gRPC SendMsg may block on this stream's HTTP/2 window. Launching
			// after the shared arbiter selects it keeps that stream from holding
			// P0/P1 selection for independent streams.
			go func() { job.result <- job.start() }()
			return nil
		})
	}()
	return d
}

func (d *sendDispatcher) send(ctx context.Context, class transport.Class, start func() error) error {
	job := sendJob{start: start, result: make(chan error, 1)}
	if err := d.arbiter.Enqueue(ctx, class, job); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return transport.Wrap(transport.StatusOf(ctx.Err()).Code, ctx.Err(), "scheduled send canceled")
	case err := <-job.result:
		return fromGRPC(err)
	}
}

func (d *sendDispatcher) stop()                              { d.arbiter.Stop() }
func (d *sendDispatcher) queueLen(class transport.Class) int { return d.arbiter.Len(class) }
