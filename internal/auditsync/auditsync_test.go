package auditsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/serverstore"
)

const (
	testAgentID   = "agent-audit-sync"
	testArchive   = "archive-audit-sync"
	testTimestamp = "2026-08-15T00:00:00.000000000Z"
)

type agentTestStream struct {
	ctx  context.Context
	sent chan producttransport.AuditUpstream
	acks chan producttransport.AuditAck
}

func (s *agentTestStream) Context() context.Context { return s.ctx }
func (s *agentTestStream) Send(message producttransport.AuditUpstream) error {
	select {
	case s.sent <- message:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (s *agentTestStream) ReceiveAck() (producttransport.AuditAck, error) {
	select {
	case ack := <-s.acks:
		return ack, nil
	case <-s.ctx.Done():
		return producttransport.AuditAck{}, s.ctx.Err()
	}
}

func TestAgentStreamsCoverageRecordsAndPersistsAcceptedACK(t *testing.T) {
	options := auditwal.DefaultOptions()
	options.SyncBytes = 1
	wal, err := auditwal.Open(filepath.Join(t.TempDir(), "wal"), testAgentID, 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	if err := wal.RebindArchive(testArchive); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), []byte(`{"kind":"MANAGED"}`)); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(AgentConfig{WAL: wal, ArchiveID: testArchive, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &agentTestStream{ctx: ctx, sent: make(chan producttransport.AuditUpstream, 4), acks: make(chan producttransport.AuditAck, 2)}
	done := make(chan error, 1)
	go func() { done <- agent.SyncAudit(ctx, producttransport.SessionInfo{AgentID: testAgentID}, stream) }()
	if message := receiveUpstream(t, stream.sent); message.Coverage == nil || message.Coverage.Revision != 0 {
		t.Fatalf("initial coverage = %#v", message)
	}
	record := receiveUpstream(t, stream.sent)
	if record.Record == nil || record.Record.Incarnation != 1 || record.Record.Sequence != 1 {
		t.Fatalf("record = %#v", record)
	}
	stream.acks <- producttransport.AuditAck{AuditArchiveID: testArchive, Incarnation: 1, Sequence: 1}
	result := receiveUpstream(t, stream.sent)
	if result.AckResult == nil || !result.AckResult.Accepted {
		t.Fatalf("ACK result = %#v", result)
	}
	bounds, err := wal.Bounds()
	if err != nil || bounds.ServerACKedThrough == nil || *bounds.ServerACKedThrough != (auditwal.Cursor{Incarnation: 1, Seq: 1}) {
		t.Fatalf("bounds = %#v, %v", bounds, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncAudit exit = %v", err)
	}
}

type staleTestWAL struct {
	ackCalls  int
	delivered bool
}

func (*staleTestWAL) Bounds() (auditwal.Bounds, error) {
	return auditwal.Bounds{WALFloor: &auditwal.Cursor{Incarnation: 1, Seq: 1},
		NextCursor: auditwal.Cursor{Incarnation: 1, Seq: 2}, AcknowledgedArchiveID: testArchive}, nil
}
func (*staleTestWAL) GetAuditCoverage() (auditwal.CoverageSnapshot, error) {
	return auditwal.CoverageSnapshot{AgentID: testAgentID}, nil
}
func (w *staleTestWAL) ReadAuditFrom(context.Context, auditwal.Cursor, int) (auditwal.ReadResult, error) {
	if w.delivered {
		return auditwal.ReadResult{}, nil
	}
	w.delivered = true
	return auditwal.ReadResult{Records: []auditwal.Record{{AgentID: testAgentID,
		Cursor: auditwal.Cursor{Incarnation: 1, Seq: 1}, AppendedAt: time.Now(), Payload: []byte(`{}`)}}}, nil
}
func (w *staleTestWAL) AckAudit(_ string, cursor auditwal.Cursor, revision uint64) error {
	w.ackCalls++
	if w.ackCalls == 1 {
		return &auditwal.StaleCoverageError{SeenRevision: revision, CurrentRevision: 1,
			Coverage: auditwal.CoverageSnapshot{AgentID: testAgentID, Revision: 1, Gaps: []auditwal.Gap{{
				Incarnation: 1, FromSeq: 1, UntilSeq: 2, Reason: auditwal.GapDiskPressure,
				Precision: auditwal.PrecisionExact, LastLossRevision: 1,
			}}}}
	}
	if revision != 1 || cursor != (auditwal.Cursor{Incarnation: 1, Seq: 1}) {
		return errors.New("unexpected retry")
	}
	return nil
}

func TestAgentReturnsStaleCoverageAndRequiresRetriedACK(t *testing.T) {
	wal := &staleTestWAL{}
	agent, err := NewAgent(AgentConfig{WAL: wal, ArchiveID: testArchive, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &agentTestStream{ctx: ctx, sent: make(chan producttransport.AuditUpstream, 5), acks: make(chan producttransport.AuditAck, 2)}
	done := make(chan error, 1)
	go func() { done <- agent.SyncAudit(ctx, producttransport.SessionInfo{AgentID: testAgentID}, stream) }()
	_ = receiveUpstream(t, stream.sent) // initial coverage
	_ = receiveUpstream(t, stream.sent) // record
	stream.acks <- producttransport.AuditAck{AuditArchiveID: testArchive, Incarnation: 1, Sequence: 1}
	rejected := receiveUpstream(t, stream.sent)
	if rejected.AckResult == nil || rejected.AckResult.Accepted || rejected.AckResult.Error != "STALE_COVERAGE" ||
		rejected.AckResult.StaleCoverage == nil || rejected.AckResult.StaleCoverage.Revision != 1 {
		t.Fatalf("stale result = %#v", rejected)
	}
	stream.acks <- producttransport.AuditAck{AuditArchiveID: testArchive, Incarnation: 1, Sequence: 1, CoverageRevisionSeen: 1}
	accepted := receiveUpstream(t, stream.sent)
	if accepted.AckResult == nil || !accepted.AckResult.Accepted || wal.ackCalls != 2 {
		t.Fatalf("accepted result = %#v calls=%d", accepted, wal.ackCalls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncAudit exit = %v", err)
	}
}

func receiveUpstream(t *testing.T, channel <-chan producttransport.AuditUpstream) producttransport.AuditUpstream {
	t.Helper()
	select {
	case message := <-channel:
		return message
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Audit upstream")
		return producttransport.AuditUpstream{}
	}
}

type serverTestStream struct {
	ctx      context.Context
	upstream chan producttransport.AuditUpstream
	acks     chan producttransport.AuditAck
}

func (s *serverTestStream) Recv(ctx context.Context) (producttransport.AuditUpstream, error) {
	select {
	case message := <-s.upstream:
		return message, nil
	case <-ctx.Done():
		return producttransport.AuditUpstream{}, ctx.Err()
	case <-s.ctx.Done():
		return producttransport.AuditUpstream{}, s.ctx.Err()
	}
}
func (s *serverTestStream) SendAck(ack producttransport.AuditAck) error {
	select {
	case s.acks <- ack:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (*serverTestStream) Close() error { return nil }

type serverTestSession struct {
	info   producttransport.SessionInfo
	stream *serverTestStream
}

func (s *serverTestSession) Info() producttransport.SessionInfo { return s.info }
func (*serverTestSession) Done() <-chan struct{}                { return make(chan struct{}) }
func (*serverTestSession) Err() error                           { return nil }
func (*serverTestSession) Close(error) error                    { return nil }
func (*serverTestSession) Heartbeat(context.Context) (producttransport.Heartbeat, error) {
	return producttransport.Heartbeat{}, nil
}
func (*serverTestSession) Query(context.Context, producttransport.QueryRequest) (producttransport.QueryResponse, error) {
	return producttransport.QueryResponse{}, nil
}
func (*serverTestSession) StartOperation(context.Context, producttransport.OperationRequest) (producttransport.OperationResponse, error) {
	return producttransport.OperationResponse{}, nil
}
func (*serverTestSession) OpenLogs(context.Context, producttransport.LogRequest) (producttransport.LogReceiveStream, error) {
	return nil, io.EOF
}
func (*serverTestSession) OpenStats(context.Context, producttransport.StatsRequest) (producttransport.StatsReceiveStream, error) {
	return nil, io.EOF
}
func (*serverTestSession) State() producttransport.State { return producttransport.StateActive }
func (*serverTestSession) LastHeartbeat() time.Time      { return time.Time{} }
func (*serverTestSession) Do(ctx context.Context, _ producttransport.TrafficClass, work func(context.Context) error) error {
	return work(ctx)
}
func (s *serverTestSession) OpenAuditSync(context.Context) (producttransport.AuditReceiveStream, error) {
	return s.stream, nil
}

func TestServerDurablyIngestsBeforeACKAndAdvancesAfterAgentAcceptance(t *testing.T) {
	ctx := context.Background()
	persistent, err := serverstore.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	if _, err := persistent.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)
	`, testAgentID, testAgentID, testTimestamp, testTimestamp); err != nil {
		t.Fatal(err)
	}
	store := auditstore.New(persistent.DB())
	server, err := NewServer(ServerConfig{Store: store, ArchiveID: testArchive,
		Decoder: EventDecoderFunc(func(_ context.Context, info producttransport.SessionInfo, record producttransport.AuditRecord) (auditstore.Event, error) {
			return auditstore.Event{AgentID: info.AgentID,
				Cursor: auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence}, OccurredAt: record.AppendedAt,
				Kind: "MANAGED", Metadata: json.RawMessage(append([]byte(nil), record.Payload...))}, nil
		}), Now: func() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	stream := &serverTestStream{ctx: runCtx, upstream: make(chan producttransport.AuditUpstream, 4), acks: make(chan producttransport.AuditAck, 1)}
	session := &serverTestSession{info: producttransport.SessionInfo{AgentID: testAgentID}, stream: stream}
	done := make(chan error, 1)
	go func() { done <- server.Run(runCtx, session) }()
	stream.upstream <- producttransport.AuditUpstream{Coverage: &producttransport.AuditCoverageSnapshot{GeneratedAt: time.Now()}}
	stream.upstream <- producttransport.AuditUpstream{Record: &producttransport.AuditRecord{
		Incarnation: 1, Sequence: 1, AppendedAt: time.Now(), Payload: []byte(`{"managed":true}`),
	}}
	var ack producttransport.AuditAck
	select {
	case ack = <-stream.acks:
	case <-time.After(time.Second):
		t.Fatal("Server did not propose ACK")
	}
	var eventCount, ackCount int
	if err := persistent.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("events before acceptance = %d, %v", eventCount, err)
	}
	if err := persistent.DB().QueryRowContext(ctx, `SELECT count(*) FROM agent_cursors WHERE acked_seq IS NOT NULL`).Scan(&ackCount); err != nil || ackCount != 0 {
		t.Fatalf("ACKs before acceptance = %d, %v", ackCount, err)
	}
	stream.upstream <- producttransport.AuditUpstream{AckResult: &producttransport.AuditAckResult{
		Proposed: producttransport.AuditCursor{Incarnation: ack.Incarnation, Sequence: ack.Sequence}, Accepted: true,
	}}
	deadline := time.Now().Add(time.Second)
	for ackCount == 0 && time.Now().Before(deadline) {
		if err := persistent.DB().QueryRowContext(ctx, `SELECT count(*) FROM agent_cursors WHERE acked_seq = 1`).Scan(&ackCount); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if ackCount != 1 {
		t.Fatal("Server did not durably advance accepted ACK")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Server Run exit = %v", err)
	}
}

func TestCanonicalDecoderPreservesExplicitManagedIndexFields(t *testing.T) {
	at := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	payload, err := auditevents.EncodeManaged(auditgen.Signal{
		ResourceType: "container", ResourceID: "container-1", Action: "restart", OccurredAt: at,
	}, "ui:127.0.0.1", "project-1", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	event, err := (CanonicalEventDecoder{}).Decode(context.Background(), producttransport.SessionInfo{AgentID: testAgentID},
		producttransport.AuditRecord{Incarnation: 2, Sequence: 9, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if event.AgentID != testAgentID || event.Cursor != (auditstore.Cursor{Incarnation: 2, Seq: 9}) ||
		event.Kind != "MANAGED" || event.Actor != "ui:127.0.0.1" || event.ProjectUID != "project-1" ||
		event.OperationID != "operation-1" || !event.OccurredAt.Equal(at) || string(event.Metadata) != string(payload) {
		t.Fatalf("decoded event = %#v", event)
	}
}
