package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

type cursorState struct {
	next         *Cursor
	ack          *Cursor
	revision     uint64
	ackUpdatedAt time.Time
}

// CoverageStart returns the immutable lower bound for one Agent in one
// canonical Archive. It is a read-only bootstrap aid for reconnecting sync
// drivers; absence is not an error.
func (s *Store) CoverageStart(ctx context.Context, archiveID, agentID string) (Cursor, CoverageStartReason, bool, error) {
	if archiveID == "" || agentID == "" {
		return Cursor{}, "", false, fmt.Errorf("%w: invalid coverage identity", ErrInvariant)
	}
	var incarnation, seq int64
	var reason string
	err := s.db.QueryRowContext(ctx, `
		SELECT from_incarnation, from_seq, reason
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type = 'LOWER_BOUND' AND source = 'SERVER_COVERAGE_START'
		  AND resolved_at IS NULL
	`, archiveID, agentID).Scan(&incarnation, &seq, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, "", false, nil
	}
	if err != nil {
		return Cursor{}, "", false, err
	}
	value := Cursor{Incarnation: uint64(incarnation), Seq: uint64(seq)}
	startReason := CoverageStartReason(reason)
	if !validCursor(value) || !validCoverageStartReason(startReason) {
		return Cursor{}, "", false, fmt.Errorf("%w: invalid persisted coverage start", ErrInvariant)
	}
	return value, startReason, true, nil
}

func (s *Store) EstablishCoverageStart(
	ctx context.Context,
	archiveID, agentID string,
	start Cursor,
	reason CoverageStartReason,
	now time.Time,
) error {
	if archiveID == "" || agentID == "" || !validCursor(start) || !validCoverageStartReason(reason) {
		return fmt.Errorf("%w: invalid coverage start", ErrInvariant)
	}
	return s.withImmediate(ctx, func(tx *connectionTx) error {
		var incarnation, seq int64
		var storedReason string
		err := tx.row(ctx, `
			SELECT from_incarnation, from_seq, reason
			FROM server_archive_coverage
			WHERE audit_archive_id = ? AND agent_id = ?
			  AND entry_type = 'LOWER_BOUND' AND source = 'SERVER_COVERAGE_START'
			  AND resolved_at IS NULL
		`, archiveID, agentID).Scan(&incarnation, &seq, &storedReason)
		if err == nil {
			if uint64(incarnation) == start.Incarnation && uint64(seq) == start.Seq && storedReason == string(reason) {
				return ensureCursorRow(ctx, tx, archiveID, agentID, start, now)
			}
			return fmt.Errorf("%w: archive coverage start changed", ErrInvariant)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.exec(ctx, `
			INSERT INTO server_archive_coverage(
				audit_archive_id, agent_id, entry_type,
				from_incarnation, from_seq, source, precision, effective,
				established_at, reason
			) VALUES (?, ?, 'LOWER_BOUND', ?, ?, 'SERVER_COVERAGE_START', 'exact', 0, ?, ?)
		`, archiveID, agentID, start.Incarnation, start.Seq, formatTime(now), string(reason)); err != nil {
			return err
		}
		return ensureCursorRow(ctx, tx, archiveID, agentID, start, now)
	})
}

func validCoverageStartReason(reason CoverageStartReason) bool {
	return reason == CoverageServerNeverHad || reason == CoverageNewAuditArchive || reason == CoverageDatabaseReinitialized
}

func ensureCursorRow(ctx context.Context, tx *connectionTx, archiveID, agentID string, start Cursor, now time.Time) error {
	_, err := tx.exec(ctx, `
		INSERT INTO agent_cursors(
			audit_archive_id, agent_id, next_incarnation, next_seq,
			coverage_revision_seen, updated_at
		) VALUES (?, ?, ?, ?, 0, ?)
		ON CONFLICT(audit_archive_id, agent_id) DO NOTHING
	`, archiveID, agentID, start.Incarnation, start.Seq, formatTime(now))
	return err
}

func (s *Store) ApplyCoverageSnapshot(
	ctx context.Context,
	archiveID string,
	snapshot CoverageSnapshot,
	now time.Time,
) (ClaimResult, error) {
	if archiveID == "" || snapshot.AgentID == "" || snapshot.Revision > math.MaxInt64 {
		return ClaimResult{}, fmt.Errorf("%w: invalid coverage snapshot", ErrInvariant)
	}
	if err := validateClaims(snapshot); err != nil {
		return ClaimResult{}, err
	}
	result := ClaimResult{}
	err := s.withImmediate(ctx, func(tx *connectionTx) error {
		state, err := loadCursorState(ctx, tx, archiveID, snapshot.AgentID)
		if err != nil {
			return err
		}
		result.CurrentRevision = state.revision
		if snapshot.Revision < state.revision {
			return &StaleClaimError{Presented: snapshot.Revision, Current: state.revision}
		}
		if snapshot.Revision == state.revision {
			equal, err := snapshotEqualsRevision(ctx, tx, snapshot)
			if err != nil {
				return err
			}
			if !equal {
				return fmt.Errorf("%w: revision %d was reused with different claims", ErrInvariant, snapshot.Revision)
			}
			return nil
		}
		for _, gap := range snapshot.Gaps {
			if _, err := tx.exec(ctx, `
				INSERT INTO agent_coverage_claims(
					agent_id, coverage_revision, claim_type, incarnation,
					from_seq, until_seq, reason, precision, reported_at
				) VALUES (?, ?, 'GAP', ?, ?, ?, ?, ?, ?)
			`, snapshot.AgentID, snapshot.Revision, gap.Incarnation,
				gap.FromSeq, gap.UntilSeq, gap.Reason, string(gap.Precision), formatTime(snapshot.GeneratedAt)); err != nil {
				return err
			}
		}
		for _, incarnation := range snapshot.CoverageUnknownIncarnations {
			if _, err := tx.exec(ctx, `
				INSERT INTO agent_coverage_claims(
					agent_id, coverage_revision, claim_type, incarnation,
					from_seq, until_seq, reason, precision, reported_at
				) VALUES (?, ?, 'COVERAGE_UNKNOWN', ?, NULL, NULL, 'COVERAGE_UNKNOWN', 'unknown', ?)
			`, snapshot.AgentID, snapshot.Revision, incarnation, formatTime(snapshot.GeneratedAt)); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, `
			UPDATE agent_cursors SET coverage_revision_seen = ?
			WHERE audit_archive_id = ? AND agent_id = ?
		`, snapshot.Revision, archiveID, snapshot.AgentID); err != nil {
			return err
		}
		if err := recomputeEffective(ctx, tx, archiveID, snapshot.AgentID, now); err != nil {
			return err
		}
		result.Applied = true
		result.CurrentRevision = snapshot.Revision
		return nil
	})
	if errors.Is(err, ErrStaleClaim) {
		s.mu.Lock()
		s.runtimeFor(archiveID, snapshot.AgentID).staleCoverageTotal++
		s.mu.Unlock()
	}
	return result, err
}

func validateClaims(snapshot CoverageSnapshot) error {
	seenUnknown := make(map[uint64]struct{})
	for _, incarnation := range snapshot.CoverageUnknownIncarnations {
		if incarnation == 0 || incarnation > math.MaxInt64 {
			return fmt.Errorf("%w: invalid unknown incarnation", ErrInvariant)
		}
		if _, exists := seenUnknown[incarnation]; exists {
			return fmt.Errorf("%w: duplicate unknown incarnation", ErrInvariant)
		}
		seenUnknown[incarnation] = struct{}{}
	}
	for _, gap := range snapshot.Gaps {
		if gap.Incarnation == 0 || gap.FromSeq == 0 || gap.FromSeq >= gap.UntilSeq ||
			gap.Incarnation > math.MaxInt64 || gap.UntilSeq > math.MaxInt64 {
			return fmt.Errorf("%w: invalid gap claim", ErrInvariant)
		}
		if gap.Precision != PrecisionExact && gap.Precision != PrecisionCoalesced {
			return fmt.Errorf("%w: invalid gap precision", ErrInvariant)
		}
		if gap.Reason == "" {
			return fmt.Errorf("%w: gap reason is empty", ErrInvariant)
		}
		if _, unknown := seenUnknown[gap.Incarnation]; unknown {
			return fmt.Errorf("%w: gap and coverage_unknown overlap", ErrInvariant)
		}
	}
	ordered := append([]GapClaim(nil), snapshot.Gaps...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Incarnation != ordered[j].Incarnation {
			return ordered[i].Incarnation < ordered[j].Incarnation
		}
		return ordered[i].FromSeq < ordered[j].FromSeq
	})
	for index := 1; index < len(ordered); index++ {
		previous, current := ordered[index-1], ordered[index]
		if previous.Incarnation == current.Incarnation && current.FromSeq < previous.UntilSeq {
			return fmt.Errorf("%w: overlapping gap claims", ErrInvariant)
		}
	}
	return nil
}

func snapshotEqualsRevision(ctx context.Context, tx *connectionTx, snapshot CoverageSnapshot) (bool, error) {
	rows, err := tx.query(ctx, `
		SELECT claim_type, incarnation, from_seq, until_seq, reason, precision
		FROM agent_coverage_claims
		WHERE agent_id = ? AND coverage_revision = ?
		ORDER BY claim_type, incarnation, from_seq
	`, snapshot.AgentID, snapshot.Revision)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var kind, reason, precision string
		var incarnation int64
		var from, until sql.NullInt64
		if err := rows.Scan(&kind, &incarnation, &from, &until, &reason, &precision); err != nil {
			return false, err
		}
		stored = append(stored, fmt.Sprintf("%s:%d:%d:%t:%d:%t:%s:%s", kind, incarnation, from.Int64, from.Valid, until.Int64, until.Valid, reason, precision))
	}
	var expected []string
	for _, gap := range snapshot.Gaps {
		expected = append(expected, fmt.Sprintf("GAP:%d:%d:true:%d:true:%s:%s",
			gap.Incarnation, gap.FromSeq, gap.UntilSeq, gap.Reason, gap.Precision))
	}
	for _, incarnation := range snapshot.CoverageUnknownIncarnations {
		expected = append(expected, fmt.Sprintf("COVERAGE_UNKNOWN:%d:0:false:0:false:COVERAGE_UNKNOWN:unknown", incarnation))
	}
	sort.Strings(expected)
	if len(stored) != len(expected) {
		return false, nil
	}
	for index := range stored {
		if stored[index] != expected[index] {
			return false, nil
		}
	}
	return true, rows.Err()
}

func loadCursorState(ctx context.Context, tx reader, archiveID, agentID string) (cursorState, error) {
	var nextInc, nextSeq, ackInc, ackSeq sql.NullInt64
	var revision int64
	var updated string
	err := tx.row(ctx, `
		SELECT next_incarnation, next_seq, acked_incarnation, acked_seq,
		       coverage_revision_seen, updated_at
		FROM agent_cursors WHERE audit_archive_id = ? AND agent_id = ?
	`, archiveID, agentID).Scan(&nextInc, &nextSeq, &ackInc, &ackSeq, &revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return cursorState{}, fmt.Errorf("%w: coverage start is not established", ErrInvariant)
	}
	if err != nil {
		return cursorState{}, err
	}
	state := cursorState{revision: uint64(revision)}
	if nextInc.Valid != nextSeq.Valid || ackInc.Valid != ackSeq.Valid {
		return cursorState{}, fmt.Errorf("%w: partial cursor", ErrInvariant)
	}
	if nextInc.Valid {
		state.next = &Cursor{uint64(nextInc.Int64), uint64(nextSeq.Int64)}
	}
	if ackInc.Valid {
		state.ack = &Cursor{uint64(ackInc.Int64), uint64(ackSeq.Int64)}
	}
	state.ackUpdatedAt, err = parseTime(updated)
	return state, err
}

func validCursor(cursor Cursor) bool {
	return cursor.Incarnation > 0 && cursor.Seq > 0 && cursor.Incarnation <= math.MaxInt64 && cursor.Seq <= math.MaxInt64
}
