package auditstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/serverstore"
)

const (
	testArchive = "archive-1"
	testAgent   = "agent-1"
)

var testEpoch = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

func TestCoverageStartReasonIsRequiredAndImmutable(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	if err := store.EstablishCoverageStart(ctx, testArchive, testAgent, Cursor{1, 1}, CoverageServerNeverHad, testEpoch); err != nil {
		t.Fatal(err)
	}
	if err := store.EstablishCoverageStart(ctx, testArchive, testAgent, Cursor{1, 1}, CoverageServerNeverHad, testEpoch); err != nil {
		t.Fatalf("idempotent coverage start: %v", err)
	}
	if err := store.EstablishCoverageStart(ctx, testArchive, testAgent, Cursor{1, 1}, CoverageNewAuditArchive, testEpoch); !errors.Is(err, ErrInvariant) {
		t.Fatalf("changed coverage reason error = %v, want ErrInvariant", err)
	}

	var reason string
	if err := db.QueryRowContext(ctx, `
		SELECT reason FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ? AND entry_type = 'LOWER_BOUND'
	`, testArchive, testAgent).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != string(CoverageServerNeverHad) {
		t.Fatalf("coverage reason = %q", reason)
	}
	start, startReason, found, err := store.CoverageStart(ctx, testArchive, testAgent)
	if err != nil || !found || start != (Cursor{1, 1}) || startReason != CoverageServerNeverHad {
		t.Fatalf("CoverageStart = %#v, %q, %v, %v", start, startReason, found, err)
	}
	if _, _, found, err := store.CoverageStart(ctx, testArchive, "missing-agent"); err != nil || found {
		t.Fatalf("missing CoverageStart = %v, %v", found, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO server_archive_coverage(
			audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
			source, precision, effective, established_at
		) VALUES (?, ?, 'LOWER_BOUND', 1, 1, 'SERVER_COVERAGE_START', 'exact', 0, ?)
	`, "archive-2", testAgent, formatTime(testEpoch)); err == nil {
		t.Fatal("lower bound without reason succeeded")
	}
}

func TestIngestIsIdempotentButRejectsConflictingDuplicate(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	event := testEvent(1, 1)
	result, err := store.Ingest(ctx, testArchive, testAgent, []Event{event}, Cursor{1, 2}, testEpoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 1 || result.Duplicates != 0 {
		t.Fatalf("first result = %+v", result)
	}
	result, err = store.Ingest(ctx, testArchive, testAgent, []Event{event}, Cursor{1, 2}, testEpoch.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 0 || result.Duplicates != 1 {
		t.Fatalf("duplicate result = %+v", result)
	}

	conflict := event
	conflict.Kind = "DIFFERENT"
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{conflict}, Cursor{1, 2}, testEpoch.Add(3*time.Second)); !errors.Is(err, ErrInvariant) {
		t.Fatalf("conflicting duplicate error = %v, want ErrInvariant", err)
	}
}

func TestConflictingBatchRollsBackEarlierInserts(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	existing := testEvent(1, 2)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{existing}, Cursor{1, 3}, testEpoch); err != nil {
		t.Fatal(err)
	}
	conflict := existing
	conflict.Kind = "DIFFERENT"
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), conflict}, Cursor{1, 3}, testEpoch.Add(time.Second)); !errors.Is(err, ErrInvariant) {
		t.Fatalf("batch error = %v, want ErrInvariant", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("record count after rollback = %d, want 1", count)
	}
}

func TestExactClaimReconcilesAgainstCanonicalAndLateRecordShrinksGap(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	_, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 3)}, Cursor{1, 4}, testEpoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(2 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 1, UntilSeq: 4, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})
	assertGaps(t, ctx, store, []Range{{From: Cursor{1, 2}, Until: Cursor{1, 3}}})

	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 2)}, Cursor{1, 4}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertGaps(t, ctx, store, nil)
	var resolved int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM server_archive_coverage
		WHERE source = 'AGENT_GAP' AND resolved_at IS NOT NULL
	`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved history rows = %d, want 1", resolved)
	}
}

func TestCoalescedClaimWaitsForTraversalAndThenShrinks(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1)}, Cursor{1, 3}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(2 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 2, UntilSeq: 5, Reason: "RETENTION", Precision: PrecisionCoalesced}},
	})
	assertGaps(t, ctx, store, nil)

	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 4)}, Cursor{1, 5}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertGaps(t, ctx, store, []Range{{From: Cursor{1, 2}, Until: Cursor{1, 4}}})
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 2)}, Cursor{1, 5}, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertGaps(t, ctx, store, []Range{{From: Cursor{1, 3}, Until: Cursor{1, 4}}})
}

func TestCoverageSnapshotRevisionIsTransactionalAndStaleIsTyped(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	snapshot := CoverageSnapshot{
		AgentID: testAgent, Revision: 2, GeneratedAt: testEpoch,
		CoverageUnknownIncarnations: []uint64{1},
	}
	result, err := store.ApplyCoverageSnapshot(ctx, testArchive, snapshot, testEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.CurrentRevision != 2 {
		t.Fatalf("result = %+v", result)
	}
	result, err = store.ApplyCoverageSnapshot(ctx, testArchive, snapshot, testEpoch)
	if err != nil || result.Applied {
		t.Fatalf("idempotent result = %+v, err = %v", result, err)
	}

	_, err = store.ApplyCoverageSnapshot(ctx, testArchive, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch,
	}, testEpoch)
	var stale *StaleClaimError
	if !errors.As(err, &stale) || stale.Current != 2 || stale.Presented != 1 {
		t.Fatalf("stale error = %#v", err)
	}
	var revision, claimCount int
	if err := db.QueryRowContext(ctx, `
		SELECT coverage_revision_seen,
		       (SELECT COUNT(*) FROM agent_coverage_claims WHERE agent_id = ?)
		FROM agent_cursors WHERE audit_archive_id = ? AND agent_id = ?
	`, testAgent, testArchive, testAgent).Scan(&revision, &claimCount); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || claimCount != 1 {
		t.Fatalf("revision=%d claims=%d", revision, claimCount)
	}
}

func TestACKRequiresCanonicalOrEffectiveCoverageAndIsMonotonic(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 3)}, Cursor{1, 4}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 0, testEpoch.Add(2*time.Second)); !errors.Is(err, ErrACKIneligible) {
		t.Fatalf("ACK with unexplained hole error = %v", err)
	}
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(3 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 2, UntilSeq: 3, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 0, testEpoch.Add(4*time.Second)); !errors.Is(err, ErrCoverageRevision) {
		t.Fatalf("revision mismatch error = %v", err)
	}
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 1, testEpoch.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced {
		t.Fatalf("ACK result = %+v", result)
	}
	result, err = store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 1, testEpoch.Add(5*time.Second))
	if err != nil || result.Advanced {
		t.Fatalf("idempotent ACK = %+v, err=%v", result, err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 2}, 1, testEpoch.Add(6*time.Second)); !errors.Is(err, ErrCursorRollback) {
		t.Fatalf("rollback error = %v", err)
	}
}

func TestACKCrossesIncarnationAfterDeliveryTraversal(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{
		testEvent(1, 1), testEvent(1, 2), testEvent(2, 1),
	}, Cursor{2, 2}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{2, 1}, 0, testEpoch.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced {
		t.Fatalf("ACK result = %+v", result)
	}
}

func TestObservationReportsBlockedWhileIngestingAndResetsAfterACK(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	observation, err := store.Observe(ctx, testArchive, testAgent, true, 0, testEpoch.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.ACKBlockedWhileIngesting || observation.ACKWatermarkStalled != 5*time.Minute ||
		observation.ACKBlockedWhileIngestingFor != 0 || observation.IngestedUnackedRecords != 1 ||
		observation.IngestedUnackedBytes <= 0 {
		t.Fatalf("blocked observation = %+v", observation)
	}
	offline, err := store.Observe(ctx, testArchive, testAgent, false, 0, testEpoch.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if offline.ACKBlockedWhileIngesting {
		t.Fatalf("offline observation = %+v", offline)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 1}, 0, testEpoch.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := store.Observe(ctx, testArchive, testAgent, true, 0, testEpoch.Add(14*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if after.ACKBlockedWhileIngesting || after.IngestedUnackedRecords != 0 {
		t.Fatalf("after ACK = %+v", after)
	}
}

func TestConcurrentDuplicateIngestProducesOneCanonicalRecord(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	const workers = 12
	start := make(chan struct{})
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Ingest(ctx, testArchive, testAgent,
				[]Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(time.Second))
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("canonical count = %d", count)
	}
}

func openAuditStore(t *testing.T) (context.Context, *Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	persistent, err := serverstore.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistent.Close() })
	if _, err := persistent.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?)
	`, testAgent, testAgent, formatTime(testEpoch), formatTime(testEpoch)); err != nil {
		t.Fatal(err)
	}
	return ctx, New(persistent.DB()), persistent.DB()
}

func establish(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO settings(key, value_json, updated_at)
		VALUES ('audit_archive_identity', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, `{"server_identity_id":"server-test","archive_generation":1,"audit_archive_id":"`+testArchive+`"}`, formatTime(testEpoch)); err != nil {
		t.Fatal(err)
	}
	if err := store.EstablishCoverageStart(ctx, testArchive, testAgent, Cursor{1, 1}, CoverageServerNeverHad, testEpoch); err != nil {
		t.Fatal(err)
	}
}

func applySnapshot(t *testing.T, ctx context.Context, store *Store, snapshot CoverageSnapshot) {
	t.Helper()
	if _, err := store.ApplyCoverageSnapshot(ctx, testArchive, snapshot, snapshot.GeneratedAt); err != nil {
		t.Fatal(err)
	}
}

func testEvent(incarnation, seq uint64) Event {
	return Event{
		AgentID: testAgent, Cursor: Cursor{incarnation, seq}, OccurredAt: testEpoch.Add(time.Duration(seq) * time.Second),
		Kind: "TEST", Metadata: json.RawMessage(fmt.Sprintf(`{"seq":%d}`, seq)),
	}
}

func assertGaps(t *testing.T, ctx context.Context, store *Store, want []Range) {
	t.Helper()
	got, err := store.EffectiveGaps(ctx, testArchive, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("gaps = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index].Range == nil || *got[index].Range != want[index] {
			t.Fatalf("gap[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}

// Compile-time assertion that the policy boundary is usable without auditstore
// selecting a retention threshold.
type noRetentionPolicy struct{}

func (noRetentionPolicy) Plan(context.Context, ArchiveUsage) (RetentionPlan, error) {
	return RetentionPlan{}, nil
}

var _ RetentionPolicy = noRetentionPolicy{}
