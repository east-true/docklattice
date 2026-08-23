package producttransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func durableTestRegistry() *SessionRegistry {
	return NewSessionRegistryWithStore(newFakeWatermarkStore())
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestReverseGRPCAuthenticatedHeartbeatAndOffline(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)}
	credential := []byte("signed-credential")
	verifier := CredentialVerifierFunc(func(_ context.Context, payload []byte, now time.Time) (CredentialIdentity, error) {
		if string(payload) != string(credential) || !now.Equal(clock.Now()) {
			t.Fatalf("verification payload=%q now=%s", payload, now)
		}
		return CredentialIdentity{AgentID: "agent-1", CredentialID: "credential-1", ServerIdentityID: "server-1"}, nil
	})
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Verifier: verifier,
		Clock: clock, OfflineAfter: 90 * time.Second, Registry: durableTestRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct {
		session ControlSession
		err     error
	}, 1)
	go func() {
		session, err := acceptor.Accept(context.Background())
		accepted <- struct {
			session ControlSession
			err     error
		}{session, err}
	}()

	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS,
		Credential: credential, Incarnation: 7, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := AgentHandlerFunc(func(_ context.Context, info SessionInfo, sentAt time.Time) (Capability, error) {
		if info.AgentID != "agent-1" || info.Incarnation != 7 || !sentAt.Equal(clock.Now()) {
			t.Fatalf("heartbeat info=%#v sent=%s", info, sentAt)
		}
		return Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true, Reason: "DEGRADED_STORAGE: FILESYSTEM_FREE_LOW"}, nil
	})
	agentSession, err := connector.Connect(context.Background(), handler)
	if err != nil {
		t.Fatal(err)
	}
	serverResult := <-accepted
	if serverResult.err != nil {
		t.Fatal(serverResult.err)
	}
	serverSession := serverResult.session
	agentInfo, serverInfo := agentSession.Info(), serverSession.Info()
	if serverInfo.SourceIP == "" {
		t.Fatalf("server did not observe the transport source IP: %#v", serverInfo)
	}
	serverInfo.SourceIP = ""
	if agentInfo != serverInfo {
		t.Fatalf("session identity differs: agent=%#v server=%#v", agentInfo, serverSession.Info())
	}
	if serverSession.Info().Incarnation != 7 || len(serverSession.Info().SessionID) != 32 {
		t.Fatalf("session info = %#v", serverSession.Info())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	heartbeat, err := serverSession.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !heartbeat.Capability.ConnectionReady || !heartbeat.Capability.DockerReady || !heartbeat.Capability.ComposeReady ||
		heartbeat.Capability.Reason != "DEGRADED_STORAGE: FILESYSTEM_FREE_LOW" || serverSession.State() != StateActive {
		t.Fatalf("heartbeat=%#v state=%s", heartbeat, serverSession.State())
	}
	clock.Advance(90 * time.Second)
	if serverSession.State() != StateOffline {
		t.Fatalf("state at threshold = %s", serverSession.State())
	}
	if _, err := serverSession.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if serverSession.State() != StateActive || !serverSession.LastHeartbeat().Equal(clock.Now()) {
		t.Fatalf("state after heartbeat=%s last=%s", serverSession.State(), serverSession.LastHeartbeat())
	}
	if _, err := serverSession.Query(ctx, QueryRequest{Kind: "containers"}); !errors.Is(err, ErrHandlerUnavailable) {
		t.Fatalf("missing query handler error = %v", err)
	}
	if _, err := serverSession.StartOperation(ctx, OperationRequest{OperationID: "operation"}); !errors.Is(err, ErrHandlerUnavailable) {
		t.Fatalf("missing operation handler error = %v", err)
	}
	operationControl, ok := serverSession.(OperationControlSession)
	if !ok {
		t.Fatal("control session does not expose operation recovery/cancellation")
	}
	if _, err := operationControl.GetOperation(ctx, GetOperationRequest{OperationID: "operation"}); !errors.Is(err, ErrHandlerUnavailable) {
		t.Fatalf("missing operation recovery handler error = %v", err)
	}
	if _, err := operationControl.CancelOperation(ctx, CancelOperationRequest{OperationID: "operation", Reason: "USER"}); !errors.Is(err, ErrHandlerUnavailable) {
		t.Fatalf("missing operation cancellation handler error = %v", err)
	}
	operationRecovery, ok := serverSession.(OperationRecoverySession)
	if !ok {
		t.Fatal("control session does not expose optional active operation recovery")
	}
	if _, err := operationRecovery.ListActiveOperations(ctx, ListActiveOperationsRequest{}); !errors.Is(err, ErrHandlerUnavailable) {
		t.Fatalf("missing active operation recovery handler error = %v", err)
	}
	_ = serverSession.Close(nil)
	_ = agentSession.Close(nil)
	_ = acceptor.Close()
}

type fullControlHandler struct {
	operationStarted chan struct{}
	operationRelease chan struct{}
	cancelCalls      *atomic.Int32
}

type auditControlHandler struct{ acknowledged chan AuditAck }

func (*auditControlHandler) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	return Capability{ConnectionReady: true}, nil
}

func (h *auditControlHandler) SyncAudit(_ context.Context, info SessionInfo, stream AuditSyncStream) error {
	if err := stream.Send(AuditUpstream{Coverage: &AuditCoverageSnapshot{
		Revision: 3, GeneratedAt: time.Unix(10, 20), Gaps: []AuditGap{{
			Incarnation: 1, FromSequence: 2, UntilSequence: 4,
			Reason: "RETENTION", Precision: "exact", LastLossRevision: 3,
		}},
	}}); err != nil {
		return err
	}
	if err := stream.Send(AuditUpstream{Record: &AuditRecord{
		Incarnation: info.Incarnation, Sequence: 7, AppendedAt: time.Unix(30, 40), Payload: []byte(`{"kind":"MANAGED"}`),
	}}); err != nil {
		return err
	}
	ack, err := stream.ReceiveAck()
	if err == nil {
		err = stream.Send(AuditUpstream{AckResult: &AuditAckResult{
			Proposed: AuditCursor{Incarnation: ack.Incarnation, Sequence: ack.Sequence}, Accepted: true,
		}})
	}
	if err == nil {
		h.acknowledged <- ack
	}
	return err
}

func TestReverseGRPCAuditSyncBidirectionalP1(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(), HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-audit", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan ControlSession, 1)
	go func() {
		session, acceptErr := acceptor.Accept(context.Background())
		if acceptErr != nil {
			t.Errorf("Accept: %v", acceptErr)
			return
		}
		accepted <- session
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("credential"), Incarnation: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &auditControlHandler{acknowledged: make(chan AuditAck, 1)}
	agentSession, err := connector.Connect(context.Background(), handler)
	if err != nil {
		t.Fatal(err)
	}
	serverSession := <-accepted
	auditSession, ok := serverSession.(AuditControlSession)
	if !ok {
		t.Fatal("control session does not expose durable audit sync")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := auditSession.OpenAuditSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := stream.Recv(ctx)
	if err != nil || coverage.Coverage == nil || coverage.Coverage.Revision != 3 ||
		len(coverage.Coverage.Gaps) != 1 || coverage.Coverage.Gaps[0].UntilSequence != 4 {
		t.Fatalf("coverage = %#v, %v", coverage, err)
	}
	record, err := stream.Recv(ctx)
	if err != nil || record.Record == nil || record.Record.Incarnation != 5 || record.Record.Sequence != 7 ||
		string(record.Record.Payload) != `{"kind":"MANAGED"}` || !record.Record.AppendedAt.Equal(time.Unix(30, 40)) {
		t.Fatalf("record = %#v, %v", record, err)
	}
	wantACK := AuditAck{AuditArchiveID: "archive-1", Incarnation: 5, Sequence: 7, CoverageRevisionSeen: 3}
	if err := stream.SendAck(wantACK); err != nil {
		t.Fatal(err)
	}
	ackResult, err := stream.Recv(ctx)
	if err != nil || ackResult.AckResult == nil || !ackResult.AckResult.Accepted ||
		ackResult.AckResult.Proposed != (AuditCursor{Incarnation: 5, Sequence: 7}) {
		t.Fatalf("ACK result = %#v, %v", ackResult, err)
	}
	select {
	case got := <-handler.acknowledged:
		if got != wantACK {
			t.Fatalf("ACK = %#v, want %#v", got, wantACK)
		}
	case <-ctx.Done():
		t.Fatal("Agent did not receive audit ACK")
	}
	_ = stream.Close()
	_ = serverSession.Close(nil)
	_ = agentSession.Close(nil)
	_ = acceptor.Close()
}

func (fullControlHandler) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	return Capability{ConnectionReady: true}, nil
}

func (fullControlHandler) Query(_ context.Context, info SessionInfo, request QueryRequest) (QueryResponse, error) {
	return QueryResponse{Payload: []byte(info.AgentID + ":" + request.Kind + ":" + request.Target + ":" + string(request.Payload))}, nil
}

func (h fullControlHandler) StartOperation(_ context.Context, _ SessionInfo, request OperationRequest) (OperationResponse, error) {
	if h.operationStarted != nil {
		close(h.operationStarted)
		go func() { <-h.operationRelease }()
	}
	return OperationResponse{
		Status: "ACCEPTED", Phase: request.Type, Revision: 9,
		PartialEffectsPossible: true, Error: "", OutputTail: append([]byte("tail:"), request.Payload...),
		OutputTruncated: true,
	}, nil
}

func (fullControlHandler) GetOperation(_ context.Context, _ SessionInfo, request GetOperationRequest) (GetOperationResponse, error) {
	if request.OperationID != "operation-1" {
		return GetOperationResponse{Found: false}, nil
	}
	return GetOperationResponse{Found: true, Operation: OperationResponse{
		Status: "running", Phase: "EXECUTING", Revision: 10, OutputTail: []byte("current"),
	}}, nil
}

func (h fullControlHandler) CancelOperation(_ context.Context, _ SessionInfo, request CancelOperationRequest) (CancelOperationResponse, error) {
	if h.cancelCalls != nil {
		h.cancelCalls.Add(1)
	}
	if request.OperationID == "missing" {
		return CancelOperationResponse{Outcome: "NOT_FOUND"}, nil
	}
	outcomes := map[string]string{
		"operation-1": "ACCEPTED", "too-late": "TOO_LATE",
		"not-cancelable": "NOT_CANCELABLE", "terminal": "ALREADY_TERMINAL",
	}
	outcome := outcomes[request.OperationID]
	if outcome == "" {
		return CancelOperationResponse{Outcome: "NOT_FOUND"}, nil
	}
	return CancelOperationResponse{Outcome: outcome, Operation: OperationResponse{
		Status: "running", Phase: "EXECUTING", Revision: 11, PartialEffectsPossible: true,
	}}, nil
}

func (fullControlHandler) ListActiveOperations(context.Context, SessionInfo, ListActiveOperationsRequest) (ListActiveOperationsResponse, error) {
	return ListActiveOperationsResponse{Operations: []ActiveOperation{
		{OperationID: "active-a", Type: "compose_up", ProjectKey: "project-a", Target: "service-a", Operation: OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 4, OutputTail: []byte("a-tail")}},
		{OperationID: "active-z", Type: "backup_restore", ProjectKey: "project-z", Operation: OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 2}},
	}}, nil
}

func TestReverseGRPCQueryAndOperationSurface(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry := durableTestRegistry()
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: registry,
		HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-rpc", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct {
		session ControlSession
		err     error
	}, 1)
	go func() {
		session, err := acceptor.Accept(context.Background())
		accepted <- struct {
			session ControlSession
			err     error
		}{session, err}
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("credential"), Incarnation: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	operationStarted := make(chan struct{})
	operationRelease := make(chan struct{})
	cancelCalls := new(atomic.Int32)
	defer close(operationRelease)
	agentSession, err := connector.Connect(context.Background(), fullControlHandler{
		operationStarted: operationStarted, operationRelease: operationRelease, cancelCalls: cancelCalls,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		t.Fatal(result.err)
	}
	serverSession := result.session

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	query, err := serverSession.Query(ctx, QueryRequest{Kind: "inspect", Target: "container-1", Payload: []byte("input")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(query.Payload), "agent-rpc:inspect:container-1:input"; got != want {
		t.Fatalf("query payload = %q, want %q", got, want)
	}
	operation, err := serverSession.StartOperation(ctx, OperationRequest{
		OperationID: "operation-1", Type: "compose_up", ProjectKey: "project", Target: "service", Payload: []byte("output"),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-operationStarted:
	default:
		t.Fatal("operation was not accepted")
	}
	if operation.Status != "ACCEPTED" || operation.Phase != "compose_up" || operation.Revision != 9 ||
		!operation.PartialEffectsPossible || string(operation.OutputTail) != "tail:output" || !operation.OutputTruncated {
		t.Fatalf("operation response = %#v", operation)
	}
	operationControl, ok := serverSession.(OperationControlSession)
	if !ok {
		t.Fatal("control session does not expose operation recovery/cancellation")
	}
	operationRecovery, ok := serverSession.(OperationRecoverySession)
	if !ok {
		t.Fatal("control session does not expose active operation recovery")
	}
	active, err := operationRecovery.ListActiveOperations(ctx, ListActiveOperationsRequest{})
	if err != nil || len(active.Operations) != 2 || active.Operations[0].OperationID != "active-a" ||
		active.Operations[1].OperationID != "active-z" || active.Operations[0].Operation.Revision != 4 ||
		string(active.Operations[0].Operation.OutputTail) != "a-tail" {
		t.Fatalf("active operation recovery = %#v, %v", active, err)
	}
	lookup, err := operationControl.GetOperation(ctx, GetOperationRequest{OperationID: "operation-1"})
	if err != nil || !lookup.Found || lookup.Operation.Status != "running" || lookup.Operation.Revision != 10 ||
		string(lookup.Operation.OutputTail) != "current" {
		t.Fatalf("operation lookup = %#v, %v", lookup, err)
	}
	missing, err := operationControl.GetOperation(ctx, GetOperationRequest{OperationID: "missing"})
	if err != nil || missing.Found {
		t.Fatalf("missing operation lookup = %#v, %v", missing, err)
	}
	canceled, err := operationControl.CancelOperation(ctx, CancelOperationRequest{OperationID: "operation-1", Reason: "TIMEOUT"})
	if err != nil || canceled.Outcome != "ACCEPTED" || canceled.Operation.Status != "running" ||
		canceled.Operation.Phase != "EXECUTING" || canceled.Operation.Revision != 11 || !canceled.Operation.PartialEffectsPossible {
		t.Fatalf("operation cancellation = %#v, %v", canceled, err)
	}
	notFound, err := operationControl.CancelOperation(ctx, CancelOperationRequest{OperationID: "missing", Reason: "USER"})
	if err != nil || notFound.Outcome != "NOT_FOUND" || notFound.Operation.Status != "" ||
		notFound.Operation.Revision != 0 || len(notFound.Operation.OutputTail) != 0 {
		t.Fatalf("missing operation cancellation = %#v, %v", notFound, err)
	}
	for operationID, want := range map[string]string{
		"too-late": "TOO_LATE", "not-cancelable": "NOT_CANCELABLE", "terminal": "ALREADY_TERMINAL",
	} {
		got, cancelErr := operationControl.CancelOperation(ctx, CancelOperationRequest{OperationID: operationID, Reason: "AGENT_SHUTDOWN"})
		if cancelErr != nil || got.Outcome != want || got.Operation.Revision != 11 {
			t.Fatalf("%s cancellation = %#v, %v", operationID, got, cancelErr)
		}
	}
	if _, err := operationControl.GetOperation(ctx, GetOperationRequest{OperationID: strings.Repeat("x", MaxOperationIDBytes+1)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized operation lookup error = %v", err)
	}
	if _, err := operationControl.CancelOperation(ctx, CancelOperationRequest{OperationID: "operation-1", Reason: "UNKNOWN"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid cancel reason error = %v", err)
	}
	current, ok := registry.Current("agent-rpc")
	if !ok || current.Info().SessionID != serverSession.Info().SessionID {
		t.Fatalf("registered current = %#v, %v", current, ok)
	}
	_ = serverSession.Close(nil)
	if got := cancelCalls.Load(); got != 5 {
		t.Fatalf("explicit cancel calls before disconnect = %d, want 5", got)
	}
	if _, ok := registry.Current("agent-rpc"); ok {
		t.Fatal("closed session remained registered")
	}
	_ = agentSession.Close(nil)
	if got := cancelCalls.Load(); got != 5 {
		t.Fatalf("transport disconnect invoked CancelOperation: calls=%d", got)
	}
	_ = acceptor.Close()
}

type boundedRecoveryHandler struct {
	fullControlHandler
	operations []ActiveOperation
}

func (handler boundedRecoveryHandler) ListActiveOperations(context.Context, SessionInfo, ListActiveOperationsRequest) (ListActiveOperationsResponse, error) {
	return ListActiveOperationsResponse{Operations: handler.operations}, nil
}

func TestListActiveOperationsRejectsCountAndResponseBounds(t *testing.T) {
	tooMany := make([]ActiveOperation, MaxActiveOperationCount+1)
	for index := range tooMany {
		tooMany[index] = ActiveOperation{OperationID: fmt.Sprintf("operation-%04d", index), Operation: OperationResponse{Status: "running"}}
	}
	service := agentService{handler: boundedRecoveryHandler{operations: tooMany}, maxMessageBytes: DefaultMaxMessageBytes}
	if _, err := service.listActiveOperations(context.Background(), nil); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over-count response error = %v", err)
	}

	oversizedTail := []ActiveOperation{{OperationID: "operation", Operation: OperationResponse{
		Status: "running", OutputTail: make([]byte, MaxOperationOutputTailBytes+1),
	}}}
	service.handler = boundedRecoveryHandler{operations: oversizedTail}
	if _, err := service.listActiveOperations(context.Background(), nil); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized-tail response error = %v", err)
	}

	service.handler = boundedRecoveryHandler{operations: []ActiveOperation{
		{OperationID: "operation", Operation: OperationResponse{Status: "running", OutputTail: make([]byte, 512)}},
	}}
	service.maxMessageBytes = 256
	if _, err := service.listActiveOperations(context.Background(), nil); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized-message response error = %v", err)
	}
}

func TestServerAcceptorAutomaticallyHeartbeats(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ticker := newFakeTicker()
	handled := make(chan struct{}, 1)
	observed := make(chan error, 1)
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, HeartbeatInterval: 30 * time.Second,
		Registry:         durableTestRegistry(),
		TickerFactory:    TickerFactoryFunc(func(time.Duration) Ticker { return ticker }),
		LivenessObserver: func(_ SessionInfo, _ State, err error) { observed <- err },
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-heartbeat", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct {
		session ControlSession
		err     error
	}, 1)
	go func() {
		session, err := acceptor.Accept(context.Background())
		accepted <- struct {
			session ControlSession
			err     error
		}{session, err}
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("credential"), Incarnation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := connector.Connect(context.Background(), AgentHandlerFunc(func(context.Context, SessionInfo, time.Time) (Capability, error) {
		handled <- struct{}{}
		return Capability{ConnectionReady: true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		t.Fatal(result.err)
	}
	select {
	case ticker.ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("automatic heartbeat loop did not consume ticker")
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("automatic heartbeat did not reach Agent")
	}
	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("automatic heartbeat = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("automatic heartbeat liveness was not observed")
	}
	_ = result.session.Close(nil)
	_ = agentSession.Close(nil)
	_ = acceptor.Close()
}

type streamingControlHandler struct {
	logsStarted    chan struct{}
	logsCanceled   chan struct{}
	logsBridge     LogRelayHandler
	statsBridge    LiveStatsHandler
	lastLogRequest LogRequest
}

func (h *streamingControlHandler) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	return Capability{ConnectionReady: true}, nil
}

func (h *streamingControlHandler) StreamLogs(ctx context.Context, _ SessionInfo, request LogRequest, sender LogSender) error {
	h.lastLogRequest = request
	if request.ContainerID == "oversize" {
		return sender.Send(LogEvent{Data: make([]byte, 2048), Stream: "STDOUT"})
	}
	return h.logsBridge.StreamLogs(ctx, SessionInfo{}, request, sender)
}

func (h *streamingControlHandler) StreamStats(ctx context.Context, _ SessionInfo, request StatsRequest, sender StatsSender) error {
	return h.statsBridge.StreamStats(ctx, SessionInfo{}, request, sender)
}

func TestReverseGRPCStreamingPriorityCancellationAndMessageLimit(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(),
		BulkConcurrency: 1, MaxMessageBytes: 1024, HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-stream", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct {
		session ControlSession
		err     error
	}, 1)
	go func() {
		session, err := acceptor.Accept(context.Background())
		accepted <- struct {
			session ControlSession
			err     error
		}{session, err}
	}()
	logsStarted := make(chan struct{})
	logsCanceled := make(chan struct{})
	logRelay, err := logrelay.New(logrelay.Config{Source: logrelay.SourceFunc(func(ctx context.Context, _ logrelay.Request, emit func(logrelay.Chunk) error) error {
		close(logsStarted)
		if err := emit(logrelay.Chunk{Data: []byte("line\n"), Stream: logrelay.Stdout, LineCount: 1}); err != nil {
			return err
		}
		<-ctx.Done()
		close(logsCanceled)
		return ctx.Err()
	})})
	if err != nil {
		t.Fatal(err)
	}
	statsStarted := make(chan struct{})
	statsCanceled := make(chan struct{})
	statsHub, err := livestats.New(livestats.Config{
		SampleInterval: time.Millisecond,
		Source: livestats.SourceFunc(func(ctx context.Context, containerID string, emit func(livestats.Sample) error) error {
			close(statsStarted)
			if err := emit(livestats.Sample{
				ContainerID: containerID, ObservedAt: time.Unix(123, 456), CPUPercent: 12.5,
				MemoryUsage: 100, MemoryLimit: 200, Health: "healthy", Uptime: 5 * time.Minute,
			}); err != nil {
				return err
			}
			<-ctx.Done()
			close(statsCanceled)
			return ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer statsHub.Close()
	handler := &streamingControlHandler{
		logsStarted: logsStarted, logsCanceled: logsCanceled, logsBridge: LogRelayHandler{Relay: logRelay},
		statsBridge: LiveStatsHandler{Hub: statsHub},
	}
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("credential"),
		Incarnation: 1, MaxMessageBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := connector.Connect(context.Background(), handler)
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		t.Fatal(result.err)
	}
	serverSession := result.session
	logCtx, cancelLog := context.WithCancel(context.Background())
	projectUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	logs, err := serverSession.OpenLogs(logCtx, LogRequest{
		ProjectUID: projectUID, Services: []string{"web"}, Follow: true, TailLines: 100,
		ShowStdout: true, ShowStderr: true, Timestamps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.logsStarted:
	case <-time.After(time.Second):
		t.Fatal("Agent log handler did not start")
	}
	if handler.lastLogRequest.ProjectUID != projectUID || handler.lastLogRequest.ContainerID != "" ||
		len(handler.lastLogRequest.Services) != 1 || handler.lastLogRequest.Services[0] != "web" ||
		handler.lastLogRequest.TailLines != 100 || !handler.lastLogRequest.ShowStdout || !handler.lastLogRequest.ShowStderr || !handler.lastLogRequest.Timestamps {
		t.Fatalf("project log wire request = %+v", handler.lastLogRequest)
	}

	// A slow P3 consumer owns the one bulk slot, while P0 remains independent.
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer blockedCancel()
	if _, err := serverSession.OpenStats(blockedCtx, StatsRequest{ContainerID: "container"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("P4 admission behind occupied bulk slot = %v", err)
	}
	heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := serverSession.Heartbeat(heartbeatCtx); err != nil {
		t.Fatalf("P0 heartbeat blocked by slow P3 stream: %v", err)
	}
	heartbeatCancel()
	logEvent, err := logs.Recv(context.Background())
	if err != nil || string(logEvent.Data) != "line\n" || logEvent.Stream != "STDOUT" {
		t.Fatalf("log event = %+v, %v", logEvent, err)
	}
	cancelLog()
	select {
	case <-handler.logsCanceled:
	case <-time.After(time.Second):
		t.Fatal("Server stream cancellation did not reach Agent handler")
	}

	statsCtx, cancelStats := context.WithCancel(context.Background())
	stats, err := serverSession.OpenStats(statsCtx, StatsRequest{ContainerID: "container"})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := stats.Recv(context.Background())
	if err != nil || sample.ContainerID != "container" || sample.CPUPercent != 12.5 || sample.MemoryUsage != 100 ||
		sample.Health != "healthy" || sample.Uptime != 5*time.Minute || !sample.ObservedAt.Equal(time.Unix(123, 456)) {
		t.Fatalf("stats sample = %+v, %v", sample, err)
	}
	select {
	case <-statsStarted:
	default:
		t.Fatal("stats source did not start")
	}
	cancelStats()
	select {
	case <-statsCanceled:
	case <-time.After(time.Second):
		t.Fatal("Server stats cancellation did not reach source")
	}

	oversize, err := serverSession.OpenLogs(context.Background(), LogRequest{ContainerID: "oversize"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oversize.Recv(context.Background()); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized stream message error = %v", err)
	}
	_ = serverSession.Close(nil)
	_ = agentSession.Close(nil)
	_ = acceptor.Close()
}

type blockingListener struct {
	mu       sync.Mutex
	calls    int
	closed   chan struct{}
	closeOne sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.closeOne.Do(func() { close(l.closed) })
	return nil
}

func (*blockingListener) Addr() net.Addr { return testNetworkAddr("blocking") }

func (l *blockingListener) acceptCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type testNetworkAddr string

func (a testNetworkAddr) Network() string { return string(a) }
func (a testNetworkAddr) String() string  { return string(a) }

func TestServerAcceptorCancellationDoesNotAccumulateAccepts(t *testing.T) {
	serverTLS, _ := testTLS(t)
	listener := newBlockingListener()
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(),
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for listener.acceptCalls() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := acceptor.Accept(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Accept %d = %v", i, err)
		}
	}
	if calls := listener.acceptCalls(); calls != 1 {
		t.Fatalf("underlying Accept calls = %d, want one broker", calls)
	}
	_ = acceptor.Close()
}

func TestProductTLSAlwaysRequiresTLS13(t *testing.T) {
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	product := productTLSConfig(base)
	if product.MinVersion != tls.VersionTLS13 || base.MinVersion != tls.VersionTLS12 {
		t.Fatalf("product minimum=%x caller minimum=%x", product.MinVersion, base.MinVersion)
	}
	_, err := NewAgentConnector(AgentConfig{
		Address: "127.0.0.1:1", TLSConfig: &tls.Config{MaxVersion: tls.VersionTLS12},
		Credential: []byte("credential"), Incarnation: 1,
	})
	if err == nil {
		t.Fatal("Agent constructor accepted TLS 1.2 maximum")
	}
	listener := newBlockingListener()
	_, err = NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: &tls.Config{MaxVersion: tls.VersionTLS12},
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{}, nil
		}),
	})
	if err == nil {
		t.Fatal("Server constructor accepted TLS 1.2 maximum")
	}
}

func TestServerAcceptorRequiresDurableIncarnationRegistry(t *testing.T) {
	serverTLS, _ := testTLS(t)
	listener := newBlockingListener()
	_, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: NewSessionRegistry(),
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{}, nil
		}),
	})
	if err == nil {
		t.Fatal("Server constructor accepted a process-local incarnation watermark")
	}
}

func TestRevokedCredentialIsRejectedBeforeSession(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(),
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{}, ErrCredentialRevoked
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	serverError := make(chan error, 1)
	go func() {
		_, err := acceptor.Accept(context.Background())
		serverError <- err
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS,
		Credential: []byte("revoked"), Incarnation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, clientErr := connector.Connect(context.Background(), AgentHandlerFunc(func(context.Context, SessionInfo, time.Time) (Capability, error) {
		return Capability{}, nil
	}))
	if clientErr == nil {
		t.Fatal("revoked credential unexpectedly connected")
	}
	if err := <-serverError; !errors.Is(err, ErrAuthentication) || !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("server error = %v", err)
	}
	_ = acceptor.Close()
}

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}),
	)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(templateFromDER(t, der))
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13},
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}
}

func templateFromDER(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
