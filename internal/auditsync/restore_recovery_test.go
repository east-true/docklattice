package auditsync

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/auditstore"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/serverstore"
)

var restoreNow = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

// restoredServer builds the state an operator's restored database is in:
// coverage established, records through (1,3), acknowledged through (1,3), and
// nothing the live system did afterwards.
func restoredServer(t *testing.T) (context.Context, *auditstore.Store, *Server) {
	t.Helper()
	ctx := context.Background()
	persistent, err := serverstore.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistent.Close() })
	if _, err := persistent.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)
	`, testAgentID, testAgentID, testTimestamp, testTimestamp); err != nil {
		t.Fatal(err)
	}
	store := auditstore.New(persistent.DB())
	if err := store.EstablishCoverageStart(ctx, testArchive, testAgentID,
		auditstore.Cursor{Incarnation: 1, Seq: 1}, auditstore.CoverageServerNeverHad, restoreNow); err != nil {
		t.Fatal(err)
	}
	var events []auditstore.Event
	for seq := uint64(1); seq <= 3; seq++ {
		events = append(events, auditstore.Event{AgentID: testAgentID,
			Cursor: auditstore.Cursor{Incarnation: 1, Seq: seq}, OccurredAt: restoreNow, Kind: "MANAGED",
			Metadata: json.RawMessage(`{}`)})
	}
	if _, err := store.Ingest(ctx, testArchive, testAgentID, events, auditstore.Cursor{Incarnation: 1, Seq: 4}, restoreNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgentID, auditstore.Cursor{Incarnation: 1, Seq: 3}, 0, restoreNow); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{Store: store, ArchiveID: testArchive,
		ServerIdentityID: "server-identity-a", ArchiveGeneration: 1,
		Decoder: EventDecoderFunc(func(_ context.Context, info producttransport.SessionInfo, record producttransport.AuditRecord) (auditstore.Event, error) {
			return auditstore.Event{AgentID: info.AgentID,
				Cursor: auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence}, OccurredAt: record.AppendedAt,
				Kind: "MANAGED", Metadata: json.RawMessage(append([]byte(nil), record.Payload...))}, nil
		}), Now: func() time.Time { return restoreNow }})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, server
}

// deliver runs one session in which the Agent resumes at resumeSeq and offers
// one record there, then accepts the Server's acknowledgement. It returns
// whatever ended the session, or nil if the session was still running.
func deliver(t *testing.T, server *Server, resume producttransport.AuditCursor) error {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &serverTestStream{ctx: runCtx, upstream: make(chan producttransport.AuditUpstream, 8),
		acks: make(chan producttransport.AuditAck, 8)}
	session := &serverTestSession{info: producttransport.SessionInfo{AgentID: testAgentID}, stream: stream}
	done := make(chan error, 1)
	go func() { done <- server.Run(runCtx, session) }()

	stream.upstream <- producttransport.AuditUpstream{Coverage: &producttransport.AuditCoverageSnapshot{GeneratedAt: restoreNow}}
	stream.upstream <- producttransport.AuditUpstream{Record: &producttransport.AuditRecord{
		Incarnation: resume.Incarnation, Sequence: resume.Sequence, AppendedAt: restoreNow, Payload: []byte(`{"managed":true}`)}}

	select {
	case err := <-done:
		t.Fatalf("session ended before acknowledging the record: %v", err)
	case <-stream.acks:
	case <-time.After(5 * time.Second):
		t.Fatal("no acknowledgement within 5s")
	}
	stream.upstream <- producttransport.AuditUpstream{AckResult: &producttransport.AuditAckResult{
		Proposed: resume, Accepted: true}}
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return nil
	}
}

// TestARestoredArchiveRecoversWhenTheAgentResumesAhead is the P1 itself. Before
// the fix this session ended with AUDIT_ACK_INELIGIBLE and did so on every
// reconnect, leaving the host OFFLINE with nothing to do about it.
func TestARestoredArchiveRecoversWhenTheAgentResumesAhead(t *testing.T) {
	ctx, store, server := restoredServer(t)

	if err := deliver(t, server, producttransport.AuditCursor{Incarnation: 1, Sequence: 20}); err != nil {
		t.Fatalf("the session ended instead of recovering: %v", err)
	}

	// The watermark moved, which is what unblocks the Agent.
	observation, err := store.Observe(ctx, testArchive, testAgentID, true, 0, restoreNow)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ACKCursor == nil || *observation.ACKCursor != (auditstore.Cursor{Incarnation: 1, Seq: 20}) {
		t.Fatalf("acknowledged cursor = %+v, want (1,20)", observation.ACKCursor)
	}

	// The loss is recorded, and recorded against the Server. An Agent gap claim
	// here would be a lie: the Agent lost nothing.
	gaps, err := store.EffectiveGaps(ctx, testArchive, testAgentID)
	if err != nil {
		t.Fatal(err)
	}
	want := auditstore.Range{From: auditstore.Cursor{Incarnation: 1, Seq: 4}, Until: auditstore.Cursor{Incarnation: 1, Seq: 20}}
	if len(gaps) != 1 || gaps[0].Range == nil || *gaps[0].Range != want {
		t.Fatalf("effective gaps = %+v, want %+v", gaps, want)
	}
	// The loss is the Server's, and the ledger has to say so.
	if source := gaps[0].Source; source != "SERVER_CURSOR_REGRESSION" {
		t.Fatalf("coverage loss attributed to %q, want SERVER_CURSOR_REGRESSION", source)
	}
}

// TestARestoredArchiveStaysRecoveredAcrossReconnects covers the reconnect loop
// the defect produced: the same recovery arriving again must not accumulate
// ledger entries.
func TestARestoredArchiveStaysRecoveredAcrossReconnects(t *testing.T) {
	ctx, store, server := restoredServer(t)
	if err := deliver(t, server, producttransport.AuditCursor{Incarnation: 1, Sequence: 20}); err != nil {
		t.Fatalf("first session: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := deliver(t, server, producttransport.AuditCursor{Incarnation: 1, Sequence: uint64(21 + attempt)}); err != nil {
			t.Fatalf("reconnect %d: %v", attempt, err)
		}
	}
	gaps, err := store.EffectiveGaps(ctx, testArchive, testAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 {
		t.Fatalf("effective gaps after reconnects = %+v, want the single original range", gaps)
	}
}

// TestAnACKBlockedAboveTheResumePointStaysBlocked is the guard that keeps this
// from becoming a general way past ACK eligibility. Here the Agent resumes at
// (1,10) and then jumps to (1,30) inside the same session, so the hole between
// them is above the point it resumed from - it is a hole in what the Agent is
// actively sending, not one the restore created. That must stay refused.
func TestAnACKBlockedAboveTheResumePointStaysBlocked(t *testing.T) {
	ctx, store, server := restoredServer(t)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &serverTestStream{ctx: runCtx, upstream: make(chan producttransport.AuditUpstream, 8),
		acks: make(chan producttransport.AuditAck, 8)}
	session := &serverTestSession{info: producttransport.SessionInfo{AgentID: testAgentID}, stream: stream}
	done := make(chan error, 1)
	go func() { done <- server.Run(runCtx, session) }()

	stream.upstream <- producttransport.AuditUpstream{Coverage: &producttransport.AuditCoverageSnapshot{GeneratedAt: restoreNow}}
	for _, sequence := range []uint64{10, 30} {
		stream.upstream <- producttransport.AuditUpstream{Record: &producttransport.AuditRecord{
			Incarnation: 1, Sequence: sequence, AppendedAt: restoreNow, Payload: []byte(`{"managed":true}`)}}
		select {
		case err := <-done:
			if sequence == 30 {
				if !errors.Is(err, auditstore.ErrACKIneligible) {
					t.Fatalf("session outcome = %v, want the ACK to stay refused", err)
				}
				goto assert
			}
			t.Fatalf("session ended at (1,%d): %v", sequence, err)
		case <-stream.acks:
		case <-time.After(5 * time.Second):
			t.Fatalf("no acknowledgement for (1,%d) within 5s", sequence)
		}
		stream.upstream <- producttransport.AuditUpstream{AckResult: &producttransport.AuditAckResult{
			Proposed: producttransport.AuditCursor{Incarnation: 1, Sequence: sequence}, Accepted: true}}
	}
	select {
	case err := <-done:
		if !errors.Is(err, auditstore.ErrACKIneligible) {
			t.Fatalf("session outcome = %v, want the ACK to stay refused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session survived an unrecoverable hole above the resume point")
	}

assert:
	// No acknowledgement may have been offered for the blocked cursor. The
	// Agent persists a proposed cursor before it answers, so an offered-then-
	// refused ACK would move where it resumes next time, and the hole this
	// refused to cover would sit below that resume position and be covered on
	// the very next reconnect. Refusing after offering is not refusing.
	select {
	case ack := <-stream.acks:
		t.Fatalf("an acknowledgement was offered for a blocked cursor: %+v", ack)
	default:
	}

	// Only the range the restore actually stranded - below where the Agent
	// resumed - may have been recorded.
	gaps, err := store.EffectiveGaps(ctx, testArchive, testAgentID)
	if err != nil {
		t.Fatal(err)
	}
	want := auditstore.Range{From: auditstore.Cursor{Incarnation: 1, Seq: 4}, Until: auditstore.Cursor{Incarnation: 1, Seq: 10}}
	if len(gaps) != 1 || gaps[0].Range == nil || *gaps[0].Range != want {
		t.Fatalf("effective gaps = %+v, want only %+v", gaps, want)
	}
}
