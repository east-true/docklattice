package serverapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/serverstore"
	"github.com/east-true/dockpilot/internal/webui"
)

const testAuditArchive = "audit-archive-test"

func TestCanonicalAuditPagesAreScopedStableCuratedAndReadOnly(t *testing.T) {
	ctx, backend, store, registry, _, secret := newAuditReadTestBackend(t)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	before := auditCanonicalState(t, ctx, store)

	first, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: 1})
	if err != nil || len(first.Events) != 1 || first.Events[0].Cursor != (webui.AuditCursor{Incarnation: 1, Seq: 1}) ||
		first.NextCursor == nil || *first.NextCursor != first.Events[0].Cursor {
		t.Fatalf("first host page = %+v, %v", first, err)
	}
	if !first.Coverage.Established || first.Coverage.ACK == nil || *first.Coverage.ACK != (webui.AuditCursor{Incarnation: 1, Seq: 1}) ||
		!first.Coverage.ACKBlockedWhileIngesting || first.Coverage.ACKWatermarkStalledSeconds < 300 ||
		len(first.Coverage.Gaps) != 1 || len(first.Coverage.UnknownIncarnations) != 1 || first.Coverage.UnknownIncarnations[0] != 3 {
		t.Fatalf("coverage = %+v", first.Coverage)
	}

	second, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Events) != 2 || second.Events[0].Cursor != (webui.AuditCursor{Incarnation: 1, Seq: 2}) ||
		second.Events[1].Cursor != (webui.AuditCursor{Incarnation: 2, Seq: 1}) || second.NextCursor != nil {
		t.Fatalf("second host page = %+v, %v", second, err)
	}
	if second.Events[1].ContinuityReason != auditevents.ContinuityReasonUncleanShutdown || second.Events[1].PreviousIncarnation != 1 {
		t.Fatalf("continuity event = %+v", second.Events[1])
	}

	activity, err := backend.ProjectActivity(ctx, "project-a", webui.AuditPageRequest{Limit: webui.DefaultAuditPageSize})
	if err != nil || activity.AgentID != "agent-a" || activity.ProjectUID != "project-a" || len(activity.Events) != 1 ||
		activity.Events[0].ProjectUID != "project-a" || activity.Events[0].OperationID != "operation-a" {
		t.Fatalf("project activity = %+v, %v", activity, err)
	}
	other, err := backend.ProjectActivity(ctx, "project-b", webui.AuditPageRequest{Limit: webui.DefaultAuditPageSize})
	if err != nil || other.AgentID != "agent-b" || len(other.Events) != 1 || other.Events[0].ResourceID != "container-b" {
		t.Fatalf("other project activity = %+v, %v", other, err)
	}

	encoded, err := json.Marshal(struct {
		Host     webui.AuditPage `json:"host"`
		Activity webui.AuditPage `json:"activity"`
	}{second, activity})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "attributes", "metadata_json", "raw_payload", "compose-content"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("curated audit response exposed %q: %s", forbidden, encoded)
		}
	}
	if after := auditCanonicalState(t, ctx, store); after != before {
		t.Fatalf("read-only audit endpoints mutated canonical tables: before=%v after=%v", before, after)
	}
}

func TestAuditWithoutCoverageStartIsEmptyAndUnestablished(t *testing.T) {
	ctx, backend, store, _ := newTestBackend(t)
	audit := auditstore.New(store.DB())
	if err := WithAuditReadModel(testAuditArchive, audit)(backend); err != nil {
		t.Fatal(err)
	}
	insertAgent(t, ctx, store, "agent-empty", "host-empty", `{}`)

	before := auditCanonicalState(t, ctx, store)
	page, err := backend.HostAudit(ctx, "agent-empty", webui.AuditPageRequest{Limit: webui.DefaultAuditPageSize})
	if err != nil {
		t.Fatalf("HostAudit without coverage start: %v", err)
	}
	if page.AgentID != "agent-empty" || page.ProjectUID != "" || len(page.Events) != 0 || page.NextCursor != nil ||
		page.Coverage.Established || page.Coverage.Start != nil || page.Coverage.ACK != nil ||
		len(page.Coverage.Gaps) != 0 || len(page.Coverage.UnknownIncarnations) != 0 {
		t.Fatalf("unestablished page = %+v", page)
	}
	if after := auditCanonicalState(t, ctx, store); after != before {
		t.Fatalf("unestablished audit read mutated canonical tables: before=%v after=%v", before, after)
	}
}

func TestAuditPagesFailClosedOnMalformedCanonicalRow(t *testing.T) {
	ctx, backend, store, _, _, _ := newAuditReadTestBackend(t)
	if _, err := store.DB().ExecContext(ctx, `
		UPDATE audit_events SET metadata_json = '{' WHERE agent_id = 'agent-a' AND incarnation = 1 AND seq = 1
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: 10}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("malformed row error = %v", err)
	}
	if _, err := backend.ProjectActivity(ctx, "project-a", webui.AuditPageRequest{Limit: 10}); err != nil {
		// The corrupt host-only event must not hide an unrelated project scope.
		t.Fatalf("scoped project activity was hidden by unrelated corrupt event: %v", err)
	}
}

func TestAuditPageFailsClosedWhenLimitPlusOneRowIsMalformed(t *testing.T) {
	ctx, backend, store, _, _, _ := newAuditReadTestBackend(t)
	if _, err := store.DB().ExecContext(ctx, `
		UPDATE audit_events SET metadata_json = '{'
		WHERE agent_id = 'agent-a' AND incarnation = 1 AND seq = 2
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: 1}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("malformed limit+1 row error = %v", err)
	}
}

func TestValidateCoverageEntryAcceptsCanonicalSourceAndReasonEnums(t *testing.T) {
	value := func(number int64) sql.NullInt64 { return sql.NullInt64{Int64: number, Valid: true} }
	reason := func(value string) sql.NullString { return sql.NullString{String: value, Valid: true} }
	tests := []struct {
		name, entryType, precision, source string
		fromSeq, untilInc, untilSeq        sql.NullInt64
		reason                             sql.NullString
	}{
		{"Agent exact gap", "GAP", "exact", "AGENT_GAP", value(2), value(1), value(4), sql.NullString{}},
		{"Agent coalesced gap", "GAP", "coalesced", "AGENT_GAP", value(2), value(1), value(4), sql.NullString{}},
		{"Agent unknown incarnation", "GAP", "unknown", "AGENT_GAP", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullString{}},
		{"continuity unknown incarnation", "GAP", "unknown", "AGENT_CONTINUITY_UNCERTAIN", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullString{}},
		{"retention applied", "GAP", "exact", "SERVER_RETENTION", value(2), value(1), value(4), reason("SERVER_RETENTION_APPLIED")},
		{"quota before ACK", "GAP", "exact", "SERVER_RETENTION", value(2), value(1), value(4), reason("QUOTA_PRESSURE_BEFORE_AGENT_ACK")},
		{"database restore", "REGRESSION", "exact", "SERVER_CURSOR_REGRESSION", value(2), value(1), value(4), reason("DATABASE_RESTORE")},
		{"archive rollback", "REGRESSION", "exact", "SERVER_CURSOR_REGRESSION", value(2), value(1), value(4), reason("ARCHIVE_ROLLBACK")},
		{"cursor metadata loss", "REGRESSION", "exact", "SERVER_CURSOR_REGRESSION", value(2), value(1), value(4), reason("CURSOR_METADATA_LOSS")},
		{"unknown regression reason", "REGRESSION", "unknown", "SERVER_CURSOR_REGRESSION", value(2), value(1), value(4), reason("UNKNOWN")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCoverageEntry(test.entryType, 1, test.fromSeq, test.untilInc, test.untilSeq,
				test.precision, test.source, test.reason); err != nil {
				t.Fatalf("canonical coverage entry rejected: %v", err)
			}
		})
	}
	if err := validateCoverageEntry("GAP", 1, value(2), value(1), value(4), "exact", "SERVER_RETENTION", reason("DATABASE_RESTORE")); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("mismatched coverage reason error = %v", err)
	}
}

func TestAuditPagesValidateIdentityAndBounds(t *testing.T) {
	ctx, backend, _, _, _, _ := newAuditReadTestBackend(t)
	tests := []struct {
		name string
		call func() error
		is   error
	}{
		{"missing agent", func() error {
			_, err := backend.HostAudit(ctx, "missing", webui.AuditPageRequest{Limit: 10})
			return err
		}, webui.ErrNotFound},
		{"missing project", func() error {
			_, err := backend.ProjectActivity(ctx, "missing", webui.AuditPageRequest{Limit: 10})
			return err
		}, webui.ErrNotFound},
		{"zero limit", func() error { _, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{}); return err }, webui.ErrInvalidRequest},
		{"large limit", func() error {
			_, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: webui.MaxAuditPageSize + 1})
			return err
		}, webui.ErrInvalidRequest},
		{"invalid cursor", func() error {
			_, err := backend.HostAudit(ctx, "agent-a", webui.AuditPageRequest{Limit: 10, Cursor: &webui.AuditCursor{Incarnation: 1}})
			return err
		}, webui.ErrInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, test.is) {
				t.Fatalf("error = %v, want %v", err, test.is)
			}
		})
	}
}

func newAuditReadTestBackend(t *testing.T) (context.Context, *Backend, *serverstore.Store, *producttransport.SessionRegistry, *auditstore.Store, string) {
	t.Helper()
	ctx, backend, store, registry := newTestBackend(t)
	audit := auditstore.New(store.DB())
	if err := WithAuditReadModel(testAuditArchive, audit)(backend); err != nil {
		t.Fatal(err)
	}
	insertAgent(t, ctx, store, "agent-a", "host-a", `{}`)
	insertAgent(t, ctx, store, "agent-b", "host-b", `{}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "Project A", `{}`)
	insertProject(t, ctx, store, "project-b", "agent-b", "Project B", `{}`)
	base := time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Second)
	secret := "compose-content-super-secret"

	if err := audit.EstablishCoverageStart(ctx, testAuditArchive, "agent-a", auditstore.Cursor{Incarnation: 1, Seq: 1}, auditstore.CoverageServerNeverHad, base); err != nil {
		t.Fatal(err)
	}
	first := encodedAuditEvent(t, "agent-a", auditstore.Cursor{Incarnation: 1, Seq: 1}, auditevents.Envelope{Event: auditgen.Event{
		Kind: auditgen.KindObserved, ResourceType: "container", ResourceID: "container-a", Action: "start",
		FirstAt: base, LastAt: base, Count: 1, Attributes: map[string]string{"detail": secret},
	}})
	if _, err := audit.Ingest(ctx, testAuditArchive, "agent-a", []auditstore.Event{first}, auditstore.Cursor{Incarnation: 1, Seq: 2}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.CheckAndAdvanceACK(ctx, testAuditArchive, "agent-a", auditstore.Cursor{Incarnation: 1, Seq: 1}, 0, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	managedEvent, err := auditgen.Managed(auditgen.Signal{
		ResourceType: "operation", ResourceID: "operation-a", Action: "completed", OccurredAt: base.Add(time.Second),
		Attributes: map[string]string{"result": "success", "raw": secret},
	}, "ui:127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	managed := encodedAuditEvent(t, "agent-a", auditstore.Cursor{Incarnation: 1, Seq: 2}, auditevents.Envelope{
		Event: managedEvent, ProjectUID: "project-a", OperationID: "operation-a",
	})
	known := uint64(2)
	continuityPayload, err := auditevents.EncodeContinuityUncertain(1, &known, base.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	continuity := storedAuditEventFromPayload(t, "agent-a", auditstore.Cursor{Incarnation: 2, Seq: 1}, continuityPayload)
	if _, err := audit.Ingest(ctx, testAuditArchive, "agent-a", []auditstore.Event{managed, continuity}, auditstore.Cursor{Incarnation: 2, Seq: 2}, base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ApplyCoverageSnapshot(ctx, testAuditArchive, auditstore.CoverageSnapshot{
		AgentID: "agent-a", Revision: 1, GeneratedAt: base.Add(3 * time.Second),
		Gaps:                        []auditstore.GapClaim{{Incarnation: 1, FromSeq: 3, UntilSeq: 5, Reason: "RETENTION", Precision: auditstore.PrecisionExact}},
		CoverageUnknownIncarnations: []uint64{3},
	}, base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := audit.EstablishCoverageStart(ctx, testAuditArchive, "agent-b", auditstore.Cursor{Incarnation: 1, Seq: 1}, auditstore.CoverageServerNeverHad, base); err != nil {
		t.Fatal(err)
	}
	other := encodedAuditEvent(t, "agent-b", auditstore.Cursor{Incarnation: 1, Seq: 1}, auditevents.Envelope{Event: auditgen.Event{
		Kind: auditgen.KindManaged, ResourceType: "container", ResourceID: "container-b", Action: "completed",
		FirstAt: base, LastAt: base, Count: 1,
	}, ProjectUID: "project-b", OperationID: "operation-b"})
	// Managed validation requires the actor to be representable but permits it
	// to remain unknown when the operation source did not provide one.
	if _, err := audit.Ingest(ctx, testAuditArchive, "agent-b", []auditstore.Event{other}, auditstore.Cursor{Incarnation: 1, Seq: 2}, base); err != nil {
		t.Fatal(err)
	}
	return ctx, backend, store, registry, audit, secret
}

func encodedAuditEvent(t *testing.T, agentID string, cursor auditstore.Cursor, envelope auditevents.Envelope) auditstore.Event {
	t.Helper()
	payload, err := auditevents.EncodeEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return storedAuditEventFromPayload(t, agentID, cursor, payload)
}

func storedAuditEventFromPayload(t *testing.T, agentID string, cursor auditstore.Cursor, payload []byte) auditstore.Event {
	t.Helper()
	envelope, err := auditevents.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return auditstore.Event{
		AgentID: agentID, Cursor: cursor, OccurredAt: envelope.Event.FirstAt, Kind: string(envelope.Event.Kind),
		Actor: envelope.Event.Actor, ProjectUID: envelope.ProjectUID, OperationID: envelope.OperationID,
		ResourceType: envelope.Event.ResourceType, ResourceID: envelope.Event.ResourceID, Action: envelope.Event.Action,
		Metadata: append(json.RawMessage(nil), payload...),
	}
}

func auditCanonicalState(t *testing.T, ctx context.Context, store *serverstore.Store) [4]string {
	t.Helper()
	queries := [...]string{
		`SELECT COALESCE(json_group_array(json_object(
			'id', id, 'agent', agent_id, 'inc', incarnation, 'seq', seq, 'at', occurred_at,
			'kind', kind, 'actor', actor, 'project', project_uid, 'operation', operation_id,
			'metadata', json_quote(CAST(metadata_json AS TEXT)))), '[]')
		 FROM (SELECT * FROM audit_events ORDER BY agent_id, incarnation, seq)`,
		`SELECT COALESCE(json_group_array(json_object(
			'id', id, 'agent', agent_id, 'revision', coverage_revision, 'type', claim_type,
			'inc', incarnation, 'from', from_seq, 'until', until_seq, 'reason', reason,
			'precision', precision, 'reported', reported_at)), '[]')
		 FROM (SELECT * FROM agent_coverage_claims ORDER BY agent_id, coverage_revision, id)`,
		`SELECT COALESCE(json_group_array(json_object(
			'id', id, 'archive', audit_archive_id, 'agent', agent_id, 'type', entry_type,
			'from_inc', from_incarnation, 'from_seq', from_seq, 'until_inc', until_incarnation,
			'until_seq', until_seq, 'source', source, 'precision', precision, 'effective', effective,
			'established', established_at, 'resolved', resolved_at, 'reason', reason)), '[]')
		 FROM (SELECT * FROM server_archive_coverage ORDER BY id)`,
		`SELECT COALESCE(json_group_array(json_object(
			'archive', audit_archive_id, 'agent', agent_id, 'next_inc', next_incarnation,
			'next_seq', next_seq, 'ack_inc', acked_incarnation, 'ack_seq', acked_seq,
			'revision', coverage_revision_seen, 'updated', updated_at)), '[]')
		 FROM (SELECT * FROM agent_cursors ORDER BY audit_archive_id, agent_id)`,
	}
	var state [len(queries)]string
	for index, query := range queries {
		if err := store.DB().QueryRowContext(ctx, query).Scan(&state[index]); err != nil {
			t.Fatal(err)
		}
	}
	return state
}
