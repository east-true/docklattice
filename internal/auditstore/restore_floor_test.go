package auditstore

import (
	"errors"
	"testing"
	"time"
)

// TestARestoreBehindTheAgentWALFloorLeavesAnUnexplainableHole characterises an
// open defect. It asserts what the code does today, not what it should do, and
// exists so that the state is described precisely rather than rediscovered from
// a flaky end-to-end failure.
//
// The situation is an operator restoring a Server database backup. The restored
// database knows it acknowledged records through some cursor. The Agent, which
// was never restored, released everything it had seen acknowledged *since* that
// backup was taken, so its WAL floor now sits above the restored delivery
// cursor. The two sides disagree about a range that no longer exists anywhere:
// the Server does not have it, and the Agent cannot resend it.
//
// Nothing in the current code closes that. The ACK is refused, the session ends,
// the Agent reconnects and is refused again, and the host stays OFFLINE - with
// no Agent-side diagnostics to say why. The end-to-end symptom is the hardening
// matrix's db-restore case failing with
// "the Agent did not reconnect to the restored Server", intermittently, because
// it depends on whether the Agent's floor happened to advance past the snapshot.
func TestARestoreBehindTheAgentWALFloorLeavesAnUnexplainableHole(t *testing.T) {
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

	// A gap entry does resolve it. That is the shape of the fix, and the whole
	// difficulty: only the Agent can claim a gap today, and it has no basis to
	// claim one for records it delivered and saw acknowledged. The Server is the
	// side that knows it will never hold this range.
	applySnapshot(t, ctx, store, CoverageSnapshot{
		AgentID: testAgent, Revision: 1, GeneratedAt: testEpoch.Add(7 * time.Second),
		Gaps: []GapClaim{{Incarnation: 1, FromSeq: 4, UntilSeq: 20, Reason: "SERVER_RESTORE", Precision: PrecisionExact}},
	})
	result, err := store.CheckAndAdvanceACK(ctx, testArchive, testAgent, Cursor{1, 21}, 1, testEpoch.Add(8*time.Second))
	if err != nil || !result.Advanced {
		t.Fatalf("ACK after an explicit gap claim = %+v, err = %v; want it to advance", result, err)
	}
}
