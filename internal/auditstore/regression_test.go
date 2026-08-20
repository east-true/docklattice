package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// restoredArchive builds the state a restored Server database is in: coverage
// established, records through (1,3), acknowledged through (1,3). Everything
// the live system did afterwards is absent, because the backup predates it.
func restoredArchive(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3)}, Cursor{1, 4}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 0, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return ctx, store
}

func ackBlocked(t *testing.T, ctx context.Context, store *Store, proposed Cursor, at time.Time) *ACKIneligibleError {
	t.Helper()
	_, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, proposed, 0, at)
	var ineligible *ACKIneligibleError
	if !errors.As(err, &ineligible) {
		t.Fatalf("ACK at (%d,%d) = %v, want it blocked", proposed.Incarnation, proposed.Seq, err)
	}
	return ineligible
}

// TestARestoreIsRecoveredWithinTheSameIncarnation is the base case: the Agent
// resumes at (1,20) because this Server once acknowledged (1,19), and the
// records between are held by neither side.
func TestARestoreIsRecoveredWithinTheSameIncarnation(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	blocked := ackBlocked(t, ctx, store, Cursor{1, 20}, testEpoch.Add(4*time.Second))
	want := Range{From: Cursor{1, 4}, Until: Cursor{1, 20}}
	if len(blocked.Unexplained) != 1 || blocked.Unexplained[0] != want {
		t.Fatalf("blocked ranges = %+v, want %+v", blocked.Unexplained, want)
	}

	recovery, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Recorded) != 1 || recovery.Recorded[0] != want {
		t.Fatalf("recorded = %+v, want %+v", recovery.Recorded, want)
	}
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 20}, 0, testEpoch.Add(6*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK after recovery = %+v, err = %v; want it to advance", result, err)
	}
	assertRegressionEntries(t, ctx, store, []Range{want})
}

// TestARestoreIsRecoveredAcrossIncarnations covers the Agent having restarted
// since the backup, which changes the incarnation but not the shape of the
// problem.
func TestARestoreIsRecoveredAcrossIncarnations(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(3, 5)}, Cursor{3, 6}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	blocked := ackBlocked(t, ctx, store, Cursor{3, 5}, testEpoch.Add(4*time.Second))
	if len(blocked.Unexplained) == 0 {
		t.Fatal("expected blocked ranges across incarnations")
	}
	recovery, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{3, 5}, Cursor{3, 5}, RegressionDatabaseRestore, testEpoch.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Recorded) == 0 {
		t.Fatal("nothing recorded for a cross-incarnation restore")
	}
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{3, 5}, 0, testEpoch.Add(6*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK after cross-incarnation recovery = %+v, err = %v", result, err)
	}
}

// TestRecoveryDoesNotCoverRecordsTheServerStillHas is the rule that keeps this
// from lying about the archive. Records inside the blocked span that survived
// the restore must stay canonical, and only the genuinely absent subsequences
// become coverage loss.
func TestRecoveryDoesNotCoverRecordsTheServerStillHas(t *testing.T) {
	ctx, store := restoredArchive(t)
	// (1,8)..(1,12) survived; the Agent resumes at (1,20).
	if _, err := store.Ingest(ctx, testArchive, testAgent, []Event{
		testEvent(1, 8), testEvent(1, 9), testEvent(1, 10), testEvent(1, 11),
	}, Cursor{1, 12}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	want := []Range{
		{From: Cursor{1, 4}, Until: Cursor{1, 8}},
		{From: Cursor{1, 12}, Until: Cursor{1, 20}},
	}
	if len(recovery.Recorded) != len(want) {
		t.Fatalf("recorded = %+v, want exactly the two absent subsequences %+v", recovery.Recorded, want)
	}
	for index := range want {
		if recovery.Recorded[index] != want[index] {
			t.Fatalf("recorded[%d] = %+v, want %+v", index, recovery.Recorded[index], want[index])
		}
	}
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 20}, 0, testEpoch.Add(6*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK after partial recovery = %+v, err = %v", result, err)
	}
}

// TestRecoveryDoesNotDuplicateExistingCoverage covers a blocked span that some
// other ledger entry already explains - here an Agent gap claim. The Agent's
// account of its own loss must not be restated as a Server one.
func TestRecoveryDoesNotDuplicateExistingCoverage(t *testing.T) {
	ctx, store := restoredArchive(t)
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(3 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 4, UntilSeq: 12, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	want := Range{From: Cursor{1, 12}, Until: Cursor{1, 20}}
	if len(recovery.Recorded) != 1 || recovery.Recorded[0] != want {
		t.Fatalf("recorded = %+v, want only the part the Agent did not claim %+v", recovery.Recorded, want)
	}
	assertRegressionEntries(t, ctx, store, []Range{want})
}

// TestRecoveryIsIdempotentAcrossReconnects is the reconnect loop: the same
// recovery arrives again and must change nothing.
func TestRecoveryIsIdempotentAcrossReconnects(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(4*time.Second))
	if err != nil || len(first.Recorded) != 1 {
		t.Fatalf("first recovery = %+v, err = %v", first, err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		again, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
			Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(time.Duration(5+attempt)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Recorded) != 0 {
			t.Fatalf("repeat %d recorded %+v, want nothing", attempt, again.Recorded)
		}
	}
	assertRegressionEntries(t, ctx, store, []Range{{From: Cursor{1, 4}, Until: Cursor{1, 20}}})
}

// TestRecoveryRefusesRangesTheAgentHasNotPassed is the guard against this
// becoming a general way around ACK eligibility. A hole the Agent might still
// fill records nothing.
func TestRecoveryRefusesRangesTheAgentHasNotPassed(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The Agent resumed at (1,10), so the hole above it is still its to fill.
	recovery, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 10}, RegressionDatabaseRestore, testEpoch.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Recorded) != 0 {
		t.Fatalf("recorded %+v for a range the Agent had not passed", recovery.Recorded)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 20}, 0, testEpoch.Add(5*time.Second)); !errors.Is(err, ErrACKIneligible) {
		t.Fatalf("ACK = %v, want it still refused", err)
	}
}

// TestRecoveryRefusesAnUnreservedReason keeps the ledger's vocabulary closed.
func TestRecoveryRefusesAnUnreservedReason(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, "BECAUSE", testEpoch.Add(3*time.Second)); !errors.Is(err, ErrInvariant) {
		t.Fatalf("unreserved reason = %v, want ErrInvariant", err)
	}
}

// TestRecoveryLeavesTheCoverageStartAlone: the archive's lower bound records
// where this archive began, and a restore does not retroactively change that.
func TestRecoveryLeavesTheCoverageStartAlone(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	start, reason, found, err := store.CoverageStart(ctx, testArchive, testAgent)
	if err != nil || !found || start != (Cursor{1, 1}) || reason != CoverageServerNeverHad {
		t.Fatalf("coverage start = %+v %q %v %v, want it unchanged at (1,1)/SERVER_NEVER_HAD", start, reason, found, err)
	}
}

func assertRegressionEntries(t *testing.T, ctx context.Context, store *Store, want []Range) {
	t.Helper()
	rows, err := store.db.QueryContext(ctx, `
		SELECT from_incarnation, from_seq, until_incarnation, until_seq, reason, precision, effective
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ? AND entry_type = 'REGRESSION' AND source = ?
		ORDER BY from_incarnation, from_seq
	`, testArchive, testAgent, regressionSource)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []Range
	for rows.Next() {
		var value Range
		var reason, precision string
		var effective int
		if err := rows.Scan(&value.From.Incarnation, &value.From.Seq,
			&value.Until.Incarnation, &value.Until.Seq, &reason, &precision, &effective); err != nil {
			t.Fatal(err)
		}
		if reason != RegressionDatabaseRestore || precision != "exact" || effective != 1 {
			t.Fatalf("entry %+v recorded as reason=%q precision=%q effective=%d", value, reason, precision, effective)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("regression entries = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("entry[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}

// TestAReadTransactionCannotWrite pins the guarantee that makes withRead safe
// to use on the Audit hot path.
//
// A deferred transaction that tries to upgrade to a write is refused outright,
// without waiting on busy_timeout - the contention defect the Server write path
// was changed for. withRead therefore hands its body a handle with no exec, so
// the compiler refuses the mistake rather than a comment asking someone not to
// make it. This fails if exec is ever added to readTx, or if reader is widened
// to include it.
func TestAReadTransactionCannotWrite(t *testing.T) {
	var tx reader = (*readTx)(nil)
	if _, writable := tx.(interface {
		exec(ctx context.Context, query string, args ...any) (sql.Result, error)
	}); writable {
		t.Fatal("a read transaction handle can execute writes; a deferred transaction that upgrades is refused without waiting")
	}
	// The immediate transaction's handle still satisfies the same read surface,
	// so helpers shared by both paths keep working.
	var _ reader = (*connectionTx)(nil)
}

// TestARegressionIsRecordedAsARegression pins the producer to the entry type
// the ledger reserved for it, and pins every reader that has to agree.
//
// entry_type says what a row is - LOWER_BOUND where coverage begins, GAP a hole
// in what was delivered, REGRESSION the archive itself having moved backwards -
// while source and reason say why. Writing a regression as a GAP made one
// source span two entry types and put the producer at odds with the API, which
// had always read effective coverage as GAP plus REGRESSION.
func TestARegressionIsRecordedAsARegression(t *testing.T) {
	ctx, store := restoredArchive(t)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20)}, Cursor{1, 21}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCursorRegression(ctx, testArchive, testAgent,
		Cursor{1, 20}, Cursor{1, 20}, RegressionDatabaseRestore, testEpoch.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}

	var entryType, source string
	if err := store.db.QueryRowContext(ctx, `
		SELECT entry_type, source FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ? AND source = ?
	`, testArchive, testAgent, regressionSource).Scan(&entryType, &source); err != nil {
		t.Fatal(err)
	}
	if entryType != "REGRESSION" {
		t.Fatalf("entry_type = %q, want REGRESSION", entryType)
	}

	// The readers that answer "is this range covered" have to see it. ACK
	// eligibility is the one that matters most: if it does not, the recovery
	// records a row and changes nothing, and the Agent stays in its loop.
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 20}, 0, testEpoch.Add(5*time.Second)); err != nil {
		t.Fatalf("ACK after a REGRESSION entry = %v, want it to advance", err)
	}
	gaps, err := store.EffectiveGaps(ctx, testArchive, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Source != regressionSource {
		t.Fatalf("effective gaps = %+v, want the regression range", gaps)
	}
	observation, err := store.Observe(ctx, testArchive, testAgent, true, 0, testEpoch.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if observation.EffectiveGapRecords == 0 {
		t.Fatal("the effective gap counter ignored a regression range")
	}
}
