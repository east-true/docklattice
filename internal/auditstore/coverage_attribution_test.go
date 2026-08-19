package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func isIneligible(err error) bool { return errors.Is(err, ErrACKIneligible) }

func coverageEntries(t *testing.T, ctx context.Context, db *sql.DB) []struct {
	Source    string
	Precision string
	From      Cursor
	Until     Cursor
	Reason    string
} {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT source, precision, from_incarnation, from_seq, until_incarnation, until_seq,
		       COALESCE(reason, '')
		FROM server_archive_coverage
		WHERE entry_type = 'GAP' AND effective = 1 AND resolved_at IS NULL
		ORDER BY from_incarnation, from_seq, source
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []struct {
		Source    string
		Precision string
		From      Cursor
		Until     Cursor
		Reason    string
	}
	for rows.Next() {
		var entry struct {
			Source    string
			Precision string
			From      Cursor
			Until     Cursor
			Reason    string
		}
		var fromInc int64
		var fromSeq, untilInc, untilSeq sql.NullInt64
		if err := rows.Scan(&entry.Source, &entry.Precision, &fromInc, &fromSeq, &untilInc, &untilSeq, &entry.Reason); err != nil {
			t.Fatal(err)
		}
		entry.From = Cursor{uint64(fromInc), uint64(fromSeq.Int64)}
		entry.Until = Cursor{uint64(untilInc.Int64), uint64(untilSeq.Int64)}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// A range the Server deleted itself and the Agent also lost must keep both
// causes. Collapsing them into one would let the Server answer "why is this
// missing" with the wrong half of the truth.
func TestServerRetentionAndAgentGapOverTheSameRangeBothSurvive(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	events := []Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3), testEvent(1, 4)}
	if _, err := store.Ingest(ctx, testArchive, testAgent, events, Cursor{1, 5}, testEpoch.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	now := testEpoch.Add(400 * 24 * time.Hour)
	if _, err := store.EnforceRetention(ctx, testArchive, fixedRetentionPolicy{plan: RetentionPlan{
		DeleteBefore: now.Add(-365 * 24 * time.Hour),
	}}, now); err != nil {
		t.Fatal(err)
	}
	assertRowCount(t, ctx, db, "audit_events", 0)

	// The Agent independently reports it lost 1..3 to disk pressure.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: now.Add(time.Minute),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 1, UntilSeq: 4, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})

	entries := coverageEntries(t, ctx, db)
	var retention, agent int
	for _, entry := range entries {
		switch entry.Source {
		case "SERVER_RETENTION":
			retention++
			if entry.Reason == "" {
				t.Fatalf("server retention entry carries no reason: %+v", entry)
			}
		case "AGENT_GAP":
			agent++
			// The ledger's reason column is Server-authored only; inventing an
			// Agent reason here is rejected by the read boundary on purpose.
			if entry.Reason != "" {
				t.Fatalf("an Agent reason was invented on the Server ledger: %+v", entry)
			}
		default:
			t.Fatalf("unexpected coverage source: %+v", entry)
		}
	}
	if retention == 0 || agent == 0 {
		t.Fatalf("one cause overwrote the other: %+v", entries)
	}
	// The Agent-authored reason survives where the Agent is the author.
	var claimReason string
	if err := db.QueryRowContext(ctx, `
		SELECT reason FROM agent_coverage_claims WHERE agent_id = ? ORDER BY coverage_revision DESC LIMIT 1
	`, testAgent).Scan(&claimReason); err != nil {
		t.Fatal(err)
	}
	if claimReason != "DISK_PRESSURE" {
		t.Fatalf("agent claim reason = %q", claimReason)
	}
}

// The Server holds the records; only the ACK is stuck. An Agent gap claim over
// the same range must not make the Server report those records as missing.
func TestAgentGapClaimDoesNotUnholdRecordsTheServerAlreadyHas(t *testing.T) {
	ctx, store, db := openAuditStore(t)
	establish(t, ctx, store)
	events := []Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3), testEvent(1, 4), testEvent(1, 5)}
	if _, err := store.Ingest(ctx, testArchive, testAgent, events, Cursor{1, 8}, testEpoch.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The Agent's WAL was evicted over 1..7 while the Server already holds 1..5.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(10 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 1, UntilSeq: 8, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})
	assertGaps(t, ctx, store, []Range{{From: Cursor{1, 6}, Until: Cursor{1, 8}}})

	// Everything through 7 is now explained, so the ACK is free to advance.
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 7}, 1, testEpoch.Add(11*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK = %+v, err = %v", result, err)
	}
	assertRowCount(t, ctx, db, "audit_events", 5)
}

// An unexplained hole below the proposed cursor must stop the ACK, and a
// later gap above it must not.
func TestACKStopsAtUnexplainedHoleButNotAtAFutureGap(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 3), testEvent(1, 4)}, Cursor{1, 9}, testEpoch.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 4}, 0, testEpoch.Add(10*time.Second)); !isIneligible(err) {
		t.Fatalf("ACK over an unexplained hole: %v", err)
	}
	// A gap claim well above the proposed cursor explains nothing below it.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(11 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 6, UntilSeq: 9, Reason: "DISK_PRESSURE", Precision: PrecisionExact}},
	})
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 4}, 1, testEpoch.Add(12*time.Second)); !isIneligible(err) {
		t.Fatalf("a future gap must not explain an earlier hole: %v", err)
	}
	// Once the hole itself is claimed, the ACK advances - and the unrelated
	// future gap never blocks it.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 2, GeneratedAt: testEpoch.Add(13 * time.Second),
		Gaps: []GapClaim{
			{Incarnation: 1, FromSeq: 2, UntilSeq: 3, Reason: "DISK_PRESSURE", Precision: PrecisionExact},
			{Incarnation: 1, FromSeq: 6, UntilSeq: 9, Reason: "DISK_PRESSURE", Precision: PrecisionExact},
		},
	})
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 4}, 2, testEpoch.Add(14*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK = %+v, err = %v", result, err)
	}
}
