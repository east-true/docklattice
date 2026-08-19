package producttransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type churnHandler struct {
	open    atomic.Int64
	closed  atomic.Int64
	release chan struct{}
}

func (h *churnHandler) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	return Capability{ConnectionReady: true}, nil
}

func (h *churnHandler) Query(_ context.Context, _ SessionInfo, request QueryRequest) (QueryResponse, error) {
	_ = request
	return QueryResponse{Payload: []byte(`{"ok":true}`)}, nil
}

// StreamLogs emits one event, then holds the stream open until the caller
// cancels it or the test releases every handler.
func (h *churnHandler) StreamLogs(ctx context.Context, _ SessionInfo, _ LogRequest, sender LogSender) error {
	h.open.Add(1)
	defer h.closed.Add(1)
	if err := sender.Send(LogEvent{Data: []byte("line\n"), Stream: "STDOUT", LineCount: 1}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.release:
		return nil
	}
}

func settledGoroutines(t *testing.T, ceiling int) int {
	t.Helper()
	best := runtime.NumGoroutine()
	for range 100 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		count := runtime.NumGoroutine()
		if count < best {
			best = count
		}
		if best <= ceiling {
			return best
		}
	}
	return best
}

func churnSession(t *testing.T, handler *churnHandler) (ControlSession, func()) {
	t.Helper()
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(),
		BulkConcurrency: 4, ProtectedConcurrency: 4, MaxMessageBytes: 1 << 20, HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-churn", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan ControlSession, 1)
	acceptErr := make(chan error, 1)
	go func() {
		session, err := acceptor.Accept(context.Background())
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- session
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("credential"),
		Incarnation: 1, MaxMessageBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := connector.Connect(context.Background(), handler)
	if err != nil {
		t.Fatal(err)
	}
	var serverSession ControlSession
	select {
	case serverSession = <-accepted:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("no session accepted")
	}
	return serverSession, func() {
		_ = serverSession.Close(nil)
		_ = agentSession.Close(nil)
		_ = acceptor.Close()
	}
}

// Opening and cancelling the same stream hundreds of times must not retain a
// goroutine, a gate slot, or a handler per iteration.
func TestRepeatedStreamOpenCancelDoesNotAccumulate(t *testing.T) {
	handler := &churnHandler{release: make(chan struct{})}
	session, stop := churnSession(t, handler)
	defer stop()
	defer close(handler.release)

	// Warm the connection and the first stream path before measuring.
	for range 3 {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := session.OpenLogs(ctx, LogRequest{ContainerID: "warm"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(context.Background()); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	baseline := settledGoroutines(t, 0)

	const iterations = 300
	for index := range iterations {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := session.OpenLogs(ctx, LogRequest{ContainerID: fmt.Sprintf("c%03d", index)})
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: open: %v", index, err)
		}
		if _, err := stream.Recv(context.Background()); err != nil {
			cancel()
			t.Fatalf("iteration %d: recv: %v", index, err)
		}
		// Cancelling is the only close contract the Server has.
		cancel()
	}
	// The gate must be free: an unreleased slot would block this open forever.
	admit, cancelAdmit := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAdmit()
	stream, err := session.OpenLogs(admit, LogRequest{ContainerID: "after-churn"})
	if err != nil {
		t.Fatalf("bulk gate did not recover after %d streams: %v", iterations, err)
	}
	if _, err := stream.Recv(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	settled := settledGoroutines(t, baseline+20)
	if settled > baseline+20 {
		t.Fatalf("goroutines after %d stream cycles = %d, baseline %d", iterations, settled, baseline)
	}
	if closed := handler.closed.Load(); closed < int64(iterations) {
		t.Fatalf("Agent handlers closed = %d, opened = %d", closed, handler.open.Load())
	}
}

// A consumer that never reads must not stall control traffic, and repeated
// Recv on an already-finished stream must stay bounded and non-racy.
func TestStalledConsumerDoesNotStarveControlOrLeakRecv(t *testing.T) {
	handler := &churnHandler{release: make(chan struct{})}
	session, stop := churnSession(t, handler)
	defer stop()
	defer close(handler.release)

	stalled := make([]LogReceiveStream, 0, 4)
	cancels := make([]context.CancelFunc, 0, 4)
	for index := range 4 {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		stream, err := session.OpenLogs(ctx, LogRequest{ContainerID: fmt.Sprintf("stall%d", index)})
		if err != nil {
			t.Fatalf("stalled stream %d: %v", index, err)
		}
		stalled = append(stalled, stream)
	}
	// Nobody reads the stalled streams, and they hold every bulk slot. P0 and
	// P1 have their own pools and must keep making progress regardless.
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := session.Heartbeat(ctx); err != nil {
			cancel()
			t.Fatalf("P0 heartbeat starved by stalled bulk consumers: %v", err)
		}
		if err := session.Do(ctx, P1DurableSync, func(context.Context) error { return nil }); err != nil {
			cancel()
			t.Fatalf("P1 durable sync starved by stalled bulk consumers: %v", err)
		}
		cancel()
	}
	// P2 deliberately shares the bulk pool, so a saturated pool must refuse it
	// on the caller's own deadline rather than queue it without bound.
	refused, cancelRefused := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := session.Do(refused, P2InteractiveQuery, func(context.Context) error { return nil })
	cancelRefused()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("P2 admission behind a saturated bulk pool = %v", err)
	}

	// Cancel one stream, then hammer Recv on it concurrently: every call must
	// return promptly instead of accumulating readers on a dead stream.
	cancels[0]()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 20 {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := stalled[0].Recv(ctx)
				cancel()
				if err == nil {
					continue
				}
				if errors.Is(err, context.DeadlineExceeded) {
					panic("Recv on a canceled stream did not return")
				}
				return
			}
		}()
	}
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("repeated Recv on a canceled stream did not settle")
	}
	for _, cancel := range cancels[1:] {
		cancel()
	}
}
