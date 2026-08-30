package agentproduct

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/producttransport"
)

type blockingMatrixSender struct {
	sent chan struct{}
}

func (s blockingMatrixSender) Send(producttransport.MetricsMatrixFrame) error {
	select {
	case s.sent <- struct{}{}:
	default:
	}
	return nil
}

func settledGoroutines(t *testing.T, ceiling int) int {
	t.Helper()
	best := runtime.NumGoroutine()
	for range 100 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		if count := runtime.NumGoroutine(); count < best {
			best = count
		}
		if best <= ceiling {
			return best
		}
	}
	return best
}

// An Agent shutting down with a viewer still attached must not wait for that
// viewer to leave. Closing the product surface ends the matrix relay, which
// unblocks the RPC handler parked on it, so the Agent's own shutdown order is
// never gated on a browser somebody left open.
func TestAgentShutdownWithAnActiveMatrixViewerDoesNotBlock(t *testing.T) {
	config, _, _ := validConfig(t)
	handler, err := New(config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	baseline := settledGoroutines(t, 0)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	sender := blockingMatrixSender{sent: make(chan struct{}, 1)}
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- handler.StreamMetricsMatrix(streamCtx, producttransport.SessionInfo{}, producttransport.MetricsMatrixRequest{}, sender)
	}()

	select {
	case <-sender.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the matrix stream never produced a frame")
	}

	closed := make(chan error, 1)
	go func() { closed <- handler.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close with an active viewer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Agent shutdown blocked on an active matrix viewer")
	}

	// The handler parked on the relay must return on its own, not because the
	// caller gave up.
	select {
	case err := <-streamDone:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			t.Fatalf("the matrix RPC ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the matrix RPC handler did not return after the Agent closed")
	}

	// Everything the relay started - the event watch, the reconcile ticker, the
	// per-container stats subscriptions - goes with it.
	if settled := settledGoroutines(t, baseline+5); settled > baseline+5 {
		t.Fatalf("goroutines after shutdown = %d, baseline %d", settled, baseline)
	}
	if err := handler.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// Closing the product surface with no viewer at all is the ordinary case and
// must be just as quiet.
func TestAgentShutdownWithoutAViewerIsClean(t *testing.T) {
	config, _, _ := validConfig(t)
	handler, err := New(config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	baseline := settledGoroutines(t, 0)
	if err := handler.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if settled := settledGoroutines(t, baseline+5); settled > baseline+5 {
		t.Fatalf("goroutines after an idle shutdown = %d, baseline %d", settled, baseline)
	}
}
