package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixedRetentionPolicy struct {
	plan RetentionPlan
	err  error
}

func (policy fixedRetentionPolicy) Plan(context.Context, ArchiveUsage) (RetentionPlan, error) {
	return policy.plan, policy.err
}

func TestDefaultRetentionPolicyThresholds(t *testing.T) {
	policy := NewDefaultRetentionPolicy()
	if policy.MaxAge != 365*24*time.Hour || policy.MaxBytes != int64(10)<<30 ||
		policy.WarningPercent != 80 || policy.AggressivePercent != 95 || policy.LowPercent != 80 {
		t.Fatalf("defaults = %+v", policy)
	}
	now := testEpoch.Add(500 * 24 * time.Hour)
	tests := []struct {
		name       string
		bytes      int64
		warning    bool
		aggressive bool
		target     int64
	}{
		{name: "normal", bytes: percentage(policy.MaxBytes, 80) - 1},
		{name: "warning", bytes: percentage(policy.MaxBytes, 80), warning: true},
		{name: "aggressive", bytes: percentage(policy.MaxBytes, 95), warning: true, aggressive: true, target: percentage(policy.MaxBytes, 80)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := policy.Plan(context.Background(), ArchiveUsage{ApproximateBytes: test.bytes, EvaluatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if plan.DeleteBefore != now.Add(-365*24*time.Hour) || plan.Warning != test.warning ||
				plan.Aggressive != test.aggressive || plan.PressureTargetBytes != test.target {
				t.Fatalf("plan = %+v", plan)
			}
		})
	}
}

func TestRetentionDeletesACKedRecordsAndPublishesMergedCoverage(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	events := []Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3)}
	if _, err := store.Ingest(ctx, testArchive, testAgent, events, Cursor{1, 4}, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 0, testEpoch.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	now := testEpoch.Add(400 * 24 * time.Hour)
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: now.Add(-365 * 24 * time.Hour),
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedACKedRecords != 3 || result.DeletedCoveredRecords != 0 ||
		result.DeletedUnACKedRecords != 0 || result.CreatedCoverageIntervals != 1 {
		t.Fatalf("result = %+v", result)
	}
	assertRetentionCoverage(t, ctx, db, retentionAppliedReason, Cursor{1, 1}, Cursor{1, 4})
	assertRowCount(t, ctx, db, "audit_events", 0)

	// Retention is not an admission gate: the next contiguous ingest still succeeds.
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{testEvent(1, 4)}, Cursor{1, 5}, now.Add(time.Second)); err != nil {
		t.Fatalf("ingest after retention: %v", err)
	}
}

func TestRetentionUsesQuotaReasonForUnACKedDeletion(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	now := testEpoch.Add(400 * 24 * time.Hour)
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: now.Add(-365 * 24 * time.Hour),
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedUnACKedRecords != 1 || result.DeletedACKedRecords != 0 {
		t.Fatalf("result = %+v", result)
	}
	assertRetentionCoverage(t, ctx, db, quotaBeforeACKReason, Cursor{1, 1}, Cursor{1, 2})
}

func TestRetentionCoverageAndDeleteAreOneTransaction(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 1}, 0, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER retention_delete_fault BEFORE DELETE ON audit_events
		BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END
	`); err != nil {
		t.Fatal(err)
	}
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(10 * time.Second),
	}}, testEpoch.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("error = %v", err)
	}
	assertRowCount(t, ctx, db, "audit_events", 1)
	var coverage int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM server_archive_coverage WHERE source = 'SERVER_RETENTION'
	`).Scan(&coverage); err != nil {
		t.Fatal(err)
	}
	if coverage != 0 {
		t.Fatalf("retention coverage rows after rollback = %d", coverage)
	}
	if result.CreatedCoverageIntervals != 0 || result.DeletedACKedRecords != 0 {
		t.Fatalf("rolled-back work reported as committed: %+v", result)
	}
}

func TestRetentionDeletesAlreadyCoveredBeforeUnACKed(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO server_archive_coverage(
			audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
			until_incarnation, until_seq, source, precision, effective, established_at, reason
		) VALUES (?, ?, 'GAP', 1, 1, 1, 2, 'SERVER_RETENTION', 'exact', 1, ?, ?)
	`, testArchive, testAgent, formatTime(testEpoch), retentionAppliedReason); err != nil {
		t.Fatal(err)
	}
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(10 * time.Second),
	}}, testEpoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedCoveredRecords != 1 || result.DeletedUnACKedRecords != 0 || result.CreatedCoverageIntervals != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestRetentionMergesAdjacentIntervals(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 2)}, Cursor{1, 3}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 2}, 0, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(2 * time.Second),
	}}, testEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(3 * time.Second),
	}}, testEpoch.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertRetentionCoverage(t, ctx, db, retentionAppliedReason, Cursor{1, 1}, Cursor{1, 3})
	var active int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM server_archive_coverage
		WHERE source = 'SERVER_RETENTION' AND effective = 1 AND resolved_at IS NULL
	`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active merged intervals = %d", active)
	}
}

func TestRetentionCompactsOnlyACKCompletedCoverageHistory(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3)}, Cursor{1, 4}, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(5 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 1, UntilSeq: 2, Reason: "old", Precision: PrecisionExact}},
	})
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 2, GeneratedAt: testEpoch.Add(6 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 2, UntilSeq: 3, Reason: "current", Precision: PrecisionExact}},
	})
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 2, testEpoch.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(10 * time.Second),
	}}, testEpoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.CompactedCoverageHistoryRows != 1 {
		t.Fatalf("compacted rows = %d", result.CompactedCoverageHistoryRows)
	}
	var revisions string
	if err := db.QueryRowContext(ctx, `SELECT group_concat(coverage_revision, ',') FROM agent_coverage_claims`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != "2" {
		t.Fatalf("remaining revisions = %q", revisions)
	}
}

func TestAggressiveRetentionStopsAtLowWatermarkBeforeUnACKedTier(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	large := testEvent(1, 1)
	large.Metadata = []byte(`{"pad":"` + strings.Repeat("x", 8192) + `"}`)
	small := testEvent(1, 2)
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{large, small}, Cursor{1, 3}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 1}, 0, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := store.ArchiveUsage(ctx, testArchive, testEpoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		Warning: true, Aggressive: true, PressureTargetBytes: before.ApproximateBytes - 1024,
	}}, testEpoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedACKedRecords != 1 || result.DeletedUnACKedRecords != 0 || !result.LowWatermarkReached {
		t.Fatalf("result = %+v", result)
	}
	assertRowCount(t, ctx, db, "audit_events", 1)
}

func TestRetentionRejectsInvalidPolicyPlan(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	usage, err := store.ArchiveUsage(ctx, testArchive, testEpoch)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		Warning: true, Aggressive: true, PressureTargetBytes: usage.ApproximateBytes,
	}}, testEpoch)
	if !errors.Is(err, ErrInvariant) {
		t.Fatalf("error = %v, want ErrInvariant", err)
	}
}

func TestRetentionRejectsNonCurrentArchive(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	_, err := store.EnforceRetention(ctx, "retired-archive", fixedRetentionPolicy{}, testEpoch)
	if !errors.Is(err, ErrInvariant) {
		t.Fatalf("error = %v, want ErrInvariant", err)
	}
}

func TestRetentionWritesOnlyCurrentArchiveCoverageWhenRetiredCursorRemains(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	if err := store.EstablishCoverageStart(ctx, "retired-archive", testAgent, Cursor{1, 1}, CoverageNewAuditArchive, testEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{testEvent(1, 1)}, Cursor{1, 2}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 1}, 0, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: testEpoch.Add(3 * time.Second),
	}}, testEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var current, retired int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM server_archive_coverage
		WHERE audit_archive_id = ? AND source = 'SERVER_RETENTION'
	`, testArchive).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM server_archive_coverage
		WHERE audit_archive_id = 'retired-archive' AND source = 'SERVER_RETENTION'
	`).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if current != 1 || retired != 0 {
		t.Fatalf("retention coverage current=%d retired=%d", current, retired)
	}
}

func assertRetentionCoverage(t *testing.T, ctx context.Context, db *sql.DB, reason string, from, until Cursor) {
	t.Helper()
	var gotReason string
	var fromIncarnation, fromSeq, untilIncarnation, untilSeq int64
	if err := db.QueryRowContext(ctx, `
		SELECT reason, from_incarnation, from_seq, until_incarnation, until_seq
		FROM server_archive_coverage
		WHERE source = 'SERVER_RETENTION' AND effective = 1 AND resolved_at IS NULL
	`).Scan(&gotReason, &fromIncarnation, &fromSeq, &untilIncarnation, &untilSeq); err != nil {
		t.Fatal(err)
	}
	if gotReason != reason || (Cursor{uint64(fromIncarnation), uint64(fromSeq)} != from) ||
		(Cursor{uint64(untilIncarnation), uint64(untilSeq)} != until) {
		t.Fatalf("coverage = %q (%d,%d)-(%d,%d), want %q %+v-%+v",
			gotReason, fromIncarnation, fromSeq, untilIncarnation, untilSeq, reason, from, until)
	}
}

func assertRowCount(t *testing.T, ctx context.Context, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
