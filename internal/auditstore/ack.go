package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CheckAndAdvanceACK advances the Server materialized ACK watermark only when
// every cursor through proposed is represented by either a canonical event or
// an effective Coverage Ledger entry. The eligibility check and cursor update
// share one IMMEDIATE transaction.
func (s *Store) CheckAndAdvanceACK(
	ctx context.Context,
	archiveID, agentID string,
	proposed Cursor,
	coverageRevisionSeen uint64,
	now time.Time,
) (ACKResult, error) {
	if archiveID == "" || agentID == "" || !validCursor(proposed) {
		return ACKResult{}, fmt.Errorf("%w: invalid ACK", ErrInvariant)
	}
	result := ACKResult{Cursor: proposed}
	err := s.withImmediate(ctx, func(tx *connectionTx) error {
		state, err := loadCursorState(ctx, tx, archiveID, agentID)
		if err != nil {
			return err
		}
		if coverageRevisionSeen != state.revision {
			return fmt.Errorf("%w: presented revision %d, persisted revision %d", ErrCoverageRevision, coverageRevisionSeen, state.revision)
		}
		if state.ack != nil {
			comparison := compareCursor(proposed, *state.ack)
			if comparison < 0 {
				return ErrCursorRollback
			}
			if comparison == 0 {
				return nil
			}
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
		if _, err := tx.exec(ctx, `
			UPDATE agent_cursors
			SET acked_incarnation = ?, acked_seq = ?, coverage_revision_seen = ?, updated_at = ?
			WHERE audit_archive_id = ? AND agent_id = ?
		`, proposed.Incarnation, proposed.Seq, coverageRevisionSeen, formatTime(now), archiveID, agentID); err != nil {
			return err
		}
		result.Advanced = true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrACKIneligible) || errors.Is(err, ErrCoverageRevision) {
			s.mu.Lock()
			s.runtimeFor(archiveID, agentID).ackRetryTotal++
			s.mu.Unlock()
		}
		return ACKResult{}, err
	}
	if result.Advanced {
		s.mu.Lock()
		runtime := s.runtimeFor(archiveID, agentID)
		runtime.ingestedSinceACK = false
		runtime.firstUnackedIngestAt = time.Time{}
		runtime.blockedSince = time.Time{}
		s.mu.Unlock()
	}
	return result, nil
}

func cursorValue(cursor *Cursor) Cursor {
	if cursor == nil {
		return Cursor{}
	}
	return *cursor
}

func unexplainedACKRanges(
	ctx context.Context,
	tx reader,
	archiveID, agentID string,
	currentACK *Cursor,
	proposed, deliveryNext Cursor,
) ([]Range, error) {
	start, err := coverageStart(ctx, tx, archiveID, agentID)
	if err != nil {
		return nil, err
	}
	if currentACK != nil && compareCursor(*currentACK, start) >= 0 {
		start = Cursor{Incarnation: currentACK.Incarnation, Seq: currentACK.Seq + 1}
	}
	if compareCursor(proposed, start) < 0 {
		return []Range{{
			From:  proposed,
			Until: Cursor{Incarnation: proposed.Incarnation, Seq: proposed.Seq + 1},
		}}, nil
	}

	incarnations, err := relevantIncarnations(ctx, tx, archiveID, agentID, start.Incarnation, proposed.Incarnation)
	if err != nil {
		return nil, err
	}
	incarnations[start.Incarnation] = struct{}{}
	incarnations[proposed.Incarnation] = struct{}{}
	ordered := make([]uint64, 0, len(incarnations))
	for incarnation := range incarnations {
		ordered = append(ordered, incarnation)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	var unexplained []Range
	for _, incarnation := range ordered {
		if incarnation < start.Incarnation || incarnation > proposed.Incarnation {
			continue
		}
		from := uint64(1)
		if incarnation == start.Incarnation {
			from = start.Seq
		}
		until, present, err := ackRangeEnd(ctx, tx, archiveID, agentID, incarnation, proposed, deliveryNext)
		if err != nil {
			return nil, err
		}
		if !present || until <= from {
			continue
		}
		missing, err := missingACKCoverage(ctx, tx, archiveID, agentID, incarnation, from, until)
		if err != nil {
			return nil, err
		}
		unexplained = append(unexplained, missing...)
	}
	return unexplained, nil
}

func coverageStart(ctx context.Context, tx reader, archiveID, agentID string) (Cursor, error) {
	var incarnation, seq int64
	err := tx.row(ctx, `
		SELECT from_incarnation, from_seq
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type = 'LOWER_BOUND' AND source = 'SERVER_COVERAGE_START'
		  AND resolved_at IS NULL
	`, archiveID, agentID).Scan(&incarnation, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, fmt.Errorf("%w: coverage start is missing", ErrInvariant)
	}
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{Incarnation: uint64(incarnation), Seq: uint64(seq)}, nil
}

func relevantIncarnations(
	ctx context.Context,
	tx reader,
	archiveID, agentID string,
	from, until uint64,
) (map[uint64]struct{}, error) {
	result := make(map[uint64]struct{})
	rows, err := tx.query(ctx, `
		SELECT incarnation FROM audit_events
		WHERE agent_id = ? AND incarnation BETWEEN ? AND ?
		UNION
		SELECT from_incarnation FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND from_incarnation BETWEEN ? AND ?
		  AND effective = 1 AND resolved_at IS NULL
	`, agentID, from, until, archiveID, agentID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var incarnation int64
		if err := rows.Scan(&incarnation); err != nil {
			return nil, err
		}
		result[uint64(incarnation)] = struct{}{}
	}
	return result, rows.Err()
}

// ackRangeEnd returns an exclusive sequence bound. Traversal into a later
// incarnation proves the previous incarnation is complete, so its last known
// canonical/coverage cursor is sufficient; an incarnation with no records is
// vacuously complete.
func ackRangeEnd(
	ctx context.Context,
	tx reader,
	archiveID, agentID string,
	incarnation uint64,
	proposed, deliveryNext Cursor,
) (uint64, bool, error) {
	if incarnation == proposed.Incarnation {
		return proposed.Seq + 1, true, nil
	}
	if incarnation >= deliveryNext.Incarnation {
		return 0, false, nil
	}
	var maximum sql.NullInt64
	err := tx.row(ctx, `
		SELECT MAX(value) FROM (
			SELECT seq AS value FROM audit_events
			WHERE agent_id = ? AND incarnation = ?
			UNION ALL
			SELECT until_seq - 1 AS value FROM server_archive_coverage
			WHERE audit_archive_id = ? AND agent_id = ?
			  AND from_incarnation = ? AND effective = 1 AND resolved_at IS NULL
			  AND until_seq IS NOT NULL
		)
	`, agentID, incarnation, archiveID, agentID, incarnation).Scan(&maximum)
	if err != nil {
		return 0, false, err
	}
	if !maximum.Valid {
		return 0, false, nil
	}
	return uint64(maximum.Int64) + 1, true, nil
}

type sequenceInterval struct{ from, until uint64 }

func missingACKCoverage(
	ctx context.Context,
	tx reader,
	archiveID, agentID string,
	incarnation, from, until uint64,
) ([]Range, error) {
	var unknown int
	if err := tx.row(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM server_archive_coverage
			WHERE audit_archive_id = ? AND agent_id = ?
			  AND entry_type = 'GAP' AND from_incarnation = ?
			  AND from_seq IS NULL AND effective = 1 AND resolved_at IS NULL
		)
	`, archiveID, agentID, incarnation).Scan(&unknown); err != nil {
		return nil, err
	}
	if unknown != 0 {
		return nil, nil
	}

	rows, err := tx.query(ctx, `
		SELECT seq, seq + 1 FROM audit_events
		WHERE agent_id = ? AND incarnation = ? AND seq >= ? AND seq < ?
		UNION ALL
		SELECT from_seq, until_seq FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type IN ('GAP', 'REGRESSION') AND from_incarnation = ?
		  AND from_seq IS NOT NULL AND effective = 1 AND resolved_at IS NULL
		  AND until_seq > ? AND from_seq < ?
		ORDER BY 1, 2
	`, agentID, incarnation, from, until, archiveID, agentID, incarnation, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intervals []sequenceInterval
	for rows.Next() {
		var interval sequenceInterval
		if err := rows.Scan(&interval.from, &interval.until); err != nil {
			return nil, err
		}
		if interval.from < from {
			interval.from = from
		}
		if interval.until > until {
			interval.until = until
		}
		intervals = append(intervals, interval)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	next := from
	var missing []Range
	for _, interval := range intervals {
		if interval.from > next {
			missing = append(missing, Range{
				From:  Cursor{Incarnation: incarnation, Seq: next},
				Until: Cursor{Incarnation: incarnation, Seq: interval.from},
			})
		}
		if interval.until > next {
			next = interval.until
		}
	}
	if next < until {
		missing = append(missing, Range{
			From:  Cursor{Incarnation: incarnation, Seq: next},
			Until: Cursor{Incarnation: incarnation, Seq: until},
		})
	}
	return missing, nil
}
