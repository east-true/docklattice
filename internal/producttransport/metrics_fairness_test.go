package producttransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// metricsFloodHandler is an Agent producing matrix frames as fast as anything
// will take them, while still serving the control plane and durable Audit. It
// is the pressure source for the fairness checks below.
type metricsFloodHandler struct {
	frames    atomic.Int64
	streams   atomic.Int64
	heartbeat atomic.Int64
	release   chan struct{}
	acked     chan AuditAck
}

func (h *metricsFloodHandler) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	h.heartbeat.Add(1)
	return Capability{ConnectionReady: true, DockerReady: true, MetricsMatrix: true}, nil
}

func (h *metricsFloodHandler) StreamMetricsMatrix(ctx context.Context, _ SessionInfo, _ MetricsMatrixRequest, sender MetricsMatrixSender) error {
	h.streams.Add(1)
	frame := MetricsMatrixFrame{
		ObservedAt: time.Unix(1700000000, 0).UTC(),
		Workload:   WorkloadSummary{CPUCapacity: 8, MemoryCapacity: 32 << 30, ContainersRunning: 50, ContainersTotal: 50},
	}
	for index := 0; index < 50; index++ {
		frame.Containers = append(frame.Containers, StatsSample{
			ContainerID: fmt.Sprintf("%064x", index), CPUPercent: float64(index), MemoryUsage: uint64(index) << 20,
		})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.release:
			return nil
		default:
		}
		if err := sender.Send(frame); err != nil {
			return err
		}
		h.frames.Add(1)
	}
}

// SyncAudit keeps offering durable records for as long as the Server keeps
// acknowledging them, so an ACK that stops advancing is visible as a stalled
// count rather than as a passing test.
func (h *metricsFloodHandler) SyncAudit(_ context.Context, info SessionInfo, stream AuditSyncStream) error {
	for sequence := uint64(1); ; sequence++ {
		if err := stream.Send(AuditUpstream{Record: &AuditRecord{
			Incarnation: info.Incarnation, Sequence: sequence, AppendedAt: time.Unix(30, 40),
			Payload: []byte(`{"kind":"MANAGED"}`),
		}}); err != nil {
			return err
		}
		ack, err := stream.ReceiveAck()
		if err != nil {
			return err
		}
		if err := stream.Send(AuditUpstream{AckResult: &AuditAckResult{
			Proposed: AuditCursor{Incarnation: ack.Incarnation, Sequence: ack.Sequence}, Accepted: true,
		}}); err != nil {
			return err
		}
		select {
		case h.acked <- ack:
		default:
		}
	}
}

func floodSession(t *testing.T, handler *metricsFloodHandler) (ControlSession, func()) {
	t.Helper()
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(),
		// The same shape the existing churn and starvation coverage uses: a
		// small shared bulk pool that P2, P3 and P4 contend for, and separate
		// protected pools for P0 and P1.
		BulkConcurrency: 4, ProtectedConcurrency: 4, MaxMessageBytes: 1 << 20, HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-flood", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan ControlSession, 1)
	acceptFailed := make(chan error, 1)
	go func() {
		session, acceptErr := acceptor.Accept(context.Background())
		if acceptErr != nil {
			acceptFailed <- acceptErr
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
	var session ControlSession
	select {
	case session = <-accepted:
	case err := <-acceptFailed:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("no session accepted")
	}
	return session, func() {
		_ = session.Close(nil)
		_ = agentSession.Close(nil)
		_ = acceptor.Close()
	}
}

// Metrics are P4. P0 control and P1 durable have their own pools and were
// already declared protected from P3/P4 starvation; this feature uses that
// policy rather than adding to it, so the criteria here are the ones the
// existing stalled-consumer coverage uses - twenty rounds, a two second
// deadline each - with the pressure coming from matrix streams instead of logs.
func TestMetricsFloodDoesNotStarveControlDurableOrAudit(t *testing.T) {
	handler := &metricsFloodHandler{release: make(chan struct{}), acked: make(chan AuditAck, 64)}
	session, stop := floodSession(t, handler)
	defer stop()
	defer close(handler.release)

	// Durable Audit is running before the flood starts and must keep advancing
	// through it.
	auditCtx, cancelAudit := context.WithCancel(context.Background())
	defer cancelAudit()
	auditSession, ok := session.(AuditControlSession)
	if !ok {
		t.Fatal("control session does not expose durable Audit sync")
	}
	audit, err := auditSession.OpenAuditSync(auditCtx)
	if err != nil {
		t.Fatalf("open audit sync: %v", err)
	}
	defer audit.Close()
	auditProgress := make(chan uint64, 1)
	auditFailed := make(chan error, 1)
	go func() {
		var last uint64
		for {
			message, err := audit.Recv(auditCtx)
			if err != nil {
				if auditCtx.Err() == nil {
					auditFailed <- err
				}
				return
			}
			if message.Record == nil {
				continue
			}
			last = message.Record.Sequence
			if err := audit.SendAck(AuditAck{
				AuditArchiveID: "archive-1", Incarnation: message.Record.Incarnation, Sequence: last,
			}); err != nil {
				if auditCtx.Err() == nil {
					auditFailed <- err
				}
				return
			}
			select {
			case auditProgress <- last:
			default:
			}
		}
	}()

	// Saturate the shared bulk pool with matrix streams nobody reads. This is
	// the worst case the design has to survive: a viewer that walked away
	// without closing.
	stalled := make([]MetricsMatrixReceiveStream, 0, 4)
	cancels := make([]context.CancelFunc, 0, 4)
	for index := 0; index < 4; index++ {
		streamCtx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		stream, err := session.OpenMetricsMatrix(streamCtx, MetricsMatrixRequest{})
		if err != nil {
			t.Fatalf("stalled matrix stream %d: %v", index, err)
		}
		stalled = append(stalled, stream)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for _, stream := range stalled {
			_ = stream.Close()
		}
	}()

	// The pressure has to be real for the rest of this test to mean anything.
	// A producer that never ran would make every check below pass trivially.
	waitFor(t, "the Agent to start producing frames", func() bool { return handler.frames.Load() > 0 })

	for round := 0; round < 20; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := session.Heartbeat(ctx); err != nil {
			cancel()
			t.Fatalf("P0 heartbeat starved by a metrics flood: %v", err)
		}
		if err := session.Do(ctx, P1DurableSync, func(context.Context) error { return nil }); err != nil {
			cancel()
			t.Fatalf("P1 durable sync starved by a metrics flood: %v", err)
		}
		cancel()
	}

	// The Audit cursor must have advanced during the flood, not merely have
	// been open. Two observations apart prove movement rather than a single
	// record that arrived before the pressure did.
	first := waitForAuditProgress(t, auditProgress, auditFailed, 0)
	second := waitForAuditProgress(t, auditProgress, auditFailed, first)
	if second <= first {
		t.Fatalf("Audit cursor did not advance during the metrics flood: %d then %d", first, second)
	}

	// P2 shares the bulk pool on purpose, so a saturated pool refuses it on the
	// caller's own deadline rather than queueing without bound. Metrics
	// saturating that pool must behave exactly as logs already do.
	refused, cancelRefused := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err = session.Do(refused, P2InteractiveQuery, func(context.Context) error { return nil })
	cancelRefused()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("P2 admission behind a bulk pool saturated by metrics = %v", err)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForAuditProgress(t *testing.T, progress <-chan uint64, failed <-chan error, above uint64) uint64 {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case sequence := <-progress:
			if sequence > above {
				return sequence
			}
		case err := <-failed:
			t.Fatalf("audit sync failed during the metrics flood: %v", err)
		case <-deadline:
			t.Fatalf("Audit cursor did not pass %d during the metrics flood", above)
		}
	}
}

// A viewer that walks away releases its stream, and the Agent stops producing.
// Nothing is collected for a stream nobody holds.
func TestMatrixStreamCancellationStopsTheAgentProducer(t *testing.T) {
	handler := &metricsFloodHandler{release: make(chan struct{}), acked: make(chan AuditAck, 1)}
	session, stop := floodSession(t, handler)
	defer stop()
	defer close(handler.release)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := session.OpenMetricsMatrix(ctx, MetricsMatrixRequest{})
	if err != nil {
		t.Fatalf("open matrix: %v", err)
	}
	recvCtx, cancelRecv := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := stream.Recv(recvCtx); err != nil {
		cancelRecv()
		t.Fatalf("first frame: %v", err)
	}
	cancelRecv()
	if handler.streams.Load() != 1 {
		t.Fatalf("Agent opened %d producers for one viewer", handler.streams.Load())
	}

	cancel()
	_ = stream.Close()

	// The Agent's producer must stop on its own once the viewer is gone. It is
	// measured by the frame counter going quiet, not by asking the Agent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		before := handler.frames.Load()
		time.Sleep(100 * time.Millisecond)
		if handler.frames.Load() == before {
			return
		}
	}
	t.Fatal("the Agent kept producing frames after its only viewer left")
}
