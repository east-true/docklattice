package auditstore

import (
	"errors"
	"testing"
	"time"
)

// TestARestoreBehindTheAgentWALFloorIsRefusedUntilItIsExplained pins the
// safety half of the restore recovery: the ACK is refused, and stays refused,
// until the loss is actually recorded. Nothing about the recovery makes
// AUDIT_ACK_INELIGIBLE softer - it only gives the one case that has a truthful
// explanation a way to record it. See regression_test.go for the recovery
// itself.
//
// The situation is an operator restoring a Server database backup. The restored
// database knows it acknowledged records through some cursor. The Agent, which
// was never restored, released everything it saw acknowledged since that backup
// was taken, so it resumes above the restored cursor. The range between exists
// nowhere: the Server does not have it, and the Agent will not send it.
func TestARestoreBehindTheAgentWALFloorIsRefusedUntilItIsExplained(t *testing.T) {
	ctx, store, _ := openAuditStore(t)
	establish(t, ctx, store)

	// The backup: records through (1,3), acknowledged.
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 1), testEvent(1, 2), testEvent(1, 3)}, Cursor{1, 4}, testEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 3}, 0, testEpoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	// The live system carried on to (1,19) and the Agent released those records
	// once they were acknowledged. The database is restored to the backup, so
	// none of that is in it; the Agent's floor is (1,20) and it offers what it
	// still holds.
	if _, err := store.Ingest(ctx, testArchive, testAgent,
		[]Event{testEvent(1, 20), testEvent(1, 21)}, Cursor{1, 22}, testEpoch.Add(3*time.Second)); err != nil {
		t.Fatalf("ingest above the restored delivery cursor: %v", err)
	}

	var ineligible *ACKIneligibleError
	_, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 21}, 0, testEpoch.Add(4*time.Second))
	if !errors.As(err, &ineligible) {
		t.Fatalf("ACK error = %v, want ACKIneligibleError", err)
	}
	want := Range{From: Cursor{1, 4}, Until: Cursor{1, 20}}
	if len(ineligible.Unexplained) != 1 || ineligible.Unexplained[0] != want {
		t.Fatalf("unexplained = %+v, want exactly %+v", ineligible.Unexplained, want)
	}

	// The Server cannot raise its own coverage start to the Agent's floor: the
	// lower bound is immutable once established, which is what makes the state
	// permanent rather than merely awkward. This is also why the one code path
	// that would have fixed it - auditsync's CursorBehindFloor branch - is gated
	// on coverage *not* already being established, and after a restore it is.
	err = store.EstablishCoverageStart(ctx, testArchive, testAgent, Cursor{1, 20}, CoverageServerNeverHad, testEpoch.Add(5*time.Second))
	if !errors.Is(err, ErrInvariant) {
		t.Fatalf("raising the coverage start to the WAL floor = %v, want ErrInvariant", err)
	}
	if _, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 21}, 0, testEpoch.Add(6*time.Second)); !errors.Is(err, ErrACKIneligible) {
		t.Fatalf("ACK after the attempted repair = %v, want it still refused", err)
	}

	// A gap entry resolves it. RecordCursorRegression is what writes one, on the
	// Server's own account rather than the Agent's; this asserts the underlying
	// eligibility rule that makes that work.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(7 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 4, UntilSeq: 20, Reason: "SERVER_RESTORE", Precision: PrecisionExact}},
	})
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 21}, 1, testEpoch.Add(8*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK after an explicit gap claim = %+v, err = %v; want it to advance", result, err)
	}
}
