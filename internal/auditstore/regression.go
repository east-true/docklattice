package auditstore

import (
	"context"
	"fmt"
	"time"
)

const (
	// regressionSource marks coverage the Server lost by going backwards in
	// time, as distinct from AGENT_GAP - which is an Agent saying it lost
	// records of its own - and from SERVER_RETENTION, which is the Server
	// discarding records on purpose. The architecture reserves this source, the
	// REGRESSION reasons, and the API rendering for exactly this case.
	regressionSource = "SERVER_CURSOR_REGRESSION"

	// RegressionDatabaseRestore is the reason for the case this exists to
	// recover: an operator restored a Server database backup, so the archive
	// went backwards while the Agent did not.
	RegressionDatabaseRestore = "DATABASE_RESTORE"
	// RegressionUnknown is what to record when the cause is not established.
	// The architecture is explicit that a guess is worse than an admission.
	RegressionUnknown = "UNKNOWN"
)

// RegressionResult reports what a recovery attempt recorded. Recording nothing
// is a normal outcome: it means the blocked range was not one the Agent had
// provably moved past, and the ACK stays refused.
type RegressionResult struct {
	Recorded []Range
}

func validRegressionReason(reason string) bool {
	switch reason {
	case RegressionDatabaseRestore, RegressionUnknown, "ARCHIVE_ROLLBACK", "CURSOR_METADATA_LOSS":
		return true
	}
	return false
}

// ACKEligibility reports whether an ACK would be accepted, without writing
// anything. It exists so the Server can decide *before* offering an
// acknowledgement the Agent will persist: the Agent stores the proposed cursor
// and only then reports acceptance, so an ACK that is offered and later refused
// still moves where that Agent resumes on its next connection.
func (s *Store) ACKEligibility(
	ctx context.Context,
	archiveID, agentID string,
	proposed Cursor,
	coverageRevisionSeen uint64,
) error {
	if archiveID == "" || agentID == "" || !validCursor(proposed) {
		return fmt.Errorf("%w: invalid ACK", ErrInvariant)
	}
	// A read transaction, not an immediate one. This runs for every Audit
	// record, and taking the write lock to answer a question that writes
	// nothing would put the whole Audit stream in line behind every other
	// writer. CheckAndAdvanceACK re-decides this under IMMEDIATE before it
	// advances anything, so a snapshot that goes stale here can only cost an
	// extra refusal - never a wrong acceptance.
	return s.withRead(ctx, func(tx reader) error {
		state, err := loadCursorState(ctx, tx, archiveID, agentID)
		if err != nil {
			return err
		}
		if coverageRevisionSeen != state.revision {
			return fmt.Errorf("%w: presented revision %d, persisted revision %d", ErrCoverageRevision, coverageRevisionSeen, state.revision)
		}
		if state.ack != nil && compareCursor(proposed, *state.ack) <= 0 {
			return nil
		}
		if state.next == nil || compareCursor(proposed, *state.next) >= 0 {
			return &ACKIneligibleError{Proposed: proposed, DeliveryNext: cursorValue(state.next)}
		}
		unexplained, err := unexplainedACKRanges(ctx, tx, archiveID, agentID, state.ack, proposed, *state.next)
		if err != nil {
			return err
		}
		if len(unexplained) != 0 {
			return &ACKIneligibleError{Proposed: proposed, DeliveryNext: *state.next, Unexplained: unexplained}
		}
		return nil
	})
}

// RecordCursorRegression records, as Server-side coverage loss, the ranges an
// ACK is blocked on when the Agent has provably moved past them.
//
// This is the recovery for a restored Server database. The restored archive
// believes it acknowledged far less than it had; the Agent, which was not
// restored, resumes from the acknowledgement the Server itself issued and has
// now forgotten. The range between the two is unobtainable: the Server does not
// hold it, and the Agent will not send it, because as far as the Agent is
// concerned the Server already confirmed it.
//
// Three things keep this from becoming a way around ACK eligibility in general:
//
//   - The blocked ranges are not supplied by the caller. They are recomputed
//     here by the same function the ACK check uses, which already subtracts
//     canonical records and existing effective coverage. A range that the
//     Server actually holds, or that some other ledger entry already explains,
//     is not in the result and so cannot be covered by this.
//   - Only ranges lying entirely below resumedAt are recorded. That is the
//     lowest cursor the Agent delivered in this session, and the Agent streams
//     strictly forward from a start it derives from its own record of the
//     Server's acknowledgement - so anything below it will not arrive.
//   - The caller has to name a reason from the reserved set. Callers that
//     cannot tell why the archive moved backwards say UNKNOWN rather than
//     assuming a restore.
//
// It is idempotent by construction. A second call with the same inputs finds
// the ranges explained by the entries the first call wrote, computes no blocked
// ranges, and writes nothing.
func (s *Store) RecordCursorRegression(
	ctx context.Context,
	archiveID, agentID string,
	proposed, resumedAt Cursor,
	reason string,
	now time.Time,
) (RegressionResult, error) {
	if archiveID == "" || agentID == "" || !validCursor(proposed) || !validCursor(resumedAt) {
		return RegressionResult{}, fmt.Errorf("%w: invalid cursor regression", ErrInvariant)
	}
	if !validRegressionReason(reason) {
		return RegressionResult{}, fmt.Errorf("%w: invalid cursor regression reason %q", ErrInvariant, reason)
	}
	var result RegressionResult
	err := s.withImmediate(ctx, func(tx *connectionTx) error {
		result = RegressionResult{}
		state, err := loadCursorState(ctx, tx, archiveID, agentID)
		if err != nil {
			return err
		}
		// Coverage has to be established already. When it is not, the archive
		// has never seen this Agent and the ordinary establish path owns the
		// lower bound - there is no regression to record.
		if _, err := coverageStart(ctx, tx, archiveID, agentID); err != nil {
			return err
		}
		if state.next == nil || compareCursor(proposed, *state.next) >= 0 {
			return nil
		}
		blocked, err := unexplainedACKRanges(ctx, tx, archiveID, agentID, state.ack, proposed, *state.next)
		if err != nil {
			return err
		}
		for _, blockedRange := range blocked {
			// Half-open throughout: a range is recordable only when its whole
			// extent is behind the point the Agent resumed from.
			if compareCursor(blockedRange.Until, resumedAt) > 0 {
				continue
			}
			if err := insertRegressionGap(ctx, tx, archiveID, agentID, blockedRange, reason, now); err != nil {
				return err
			}
			result.Recorded = append(result.Recorded, blockedRange)
		}
		return nil
	})
	if err != nil {
		return RegressionResult{}, err
	}
	return result, nil
}

// insertRegressionGap writes one half-open range. The ledger stores a range per
// incarnation, and unexplainedACKRanges never spans incarnations in a single
// range, so this does not have to split.
func insertRegressionGap(
	ctx context.Context,
	tx *connectionTx,
	archiveID, agentID string,
	value Range,
	reason string,
	now time.Time,
) error {
	if value.From.Incarnation != value.Until.Incarnation {
		return fmt.Errorf("%w: cursor regression range spans incarnations", ErrInvariant)
	}
	_, err := tx.exec(ctx, `
		INSERT INTO server_archive_coverage(
			audit_archive_id, agent_id, entry_type,
			from_incarnation, from_seq, until_incarnation, until_seq,
			source, precision, effective, established_at, reason
		) VALUES (?, ?, 'GAP', ?, ?, ?, ?, ?, 'exact', 1, ?, ?)
	`, archiveID, agentID, value.From.Incarnation, value.From.Seq,
		value.Until.Incarnation, value.Until.Seq, regressionSource, formatTime(now), reason)
	return err
}
