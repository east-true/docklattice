package auditstore

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

type desiredCoverage struct {
	unknownIncarnation uint64
	rangeValue         *Range
	precision          Precision
}

func recomputeEffective(ctx context.Context, tx *connectionTx, archiveID, agentID string, now time.Time) error {
	state, err := loadCursorState(ctx, tx, archiveID, agentID)
	if err != nil {
		return err
	}
	claims, unknown, err := loadLatestClaims(ctx, tx, agentID, state.revision)
	if err != nil {
		return err
	}
	desired := make([]desiredCoverage, 0)
	for _, gap := range claims {
		if gap.Precision == PrecisionCoalesced {
			end := Cursor{gap.Incarnation, gap.UntilSeq}
			if state.next == nil || compareCursor(*state.next, end) < 0 {
				continue
			}
		}
		missing, err := missingRanges(ctx, tx, agentID, gap)
		if err != nil {
			return err
		}
		for _, interval := range missing {
			value := interval
			desired = append(desired, desiredCoverage{rangeValue: &value, precision: gap.Precision})
		}
	}
	for _, incarnation := range unknown {
		desired = append(desired, desiredCoverage{unknownIncarnation: incarnation, precision: PrecisionUnknown})
	}
	sortDesired(desired)
	current, err := loadCurrentEffective(ctx, tx, archiveID, agentID)
	if err != nil {
		return err
	}
	if equalDesired(current, desired) {
		return nil
	}
	if _, err := tx.exec(ctx, `
		UPDATE server_archive_coverage SET resolved_at = ?
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type = 'GAP' AND source = 'AGENT_GAP'
		  AND effective = 1 AND resolved_at IS NULL
	`, formatTime(now), archiveID, agentID); err != nil {
		return err
	}
	for _, entry := range desired {
		if entry.rangeValue == nil {
			if _, err := tx.exec(ctx, `
				INSERT INTO server_archive_coverage(
					audit_archive_id, agent_id, entry_type, from_incarnation,
					source, precision, effective, established_at
				) VALUES (?, ?, 'GAP', ?, 'AGENT_GAP', 'unknown', 1, ?)
			`, archiveID, agentID, entry.unknownIncarnation, formatTime(now)); err != nil {
				return err
			}
			continue
		}
		interval := entry.rangeValue
		if _, err := tx.exec(ctx, `
			INSERT INTO server_archive_coverage(
				audit_archive_id, agent_id, entry_type,
				from_incarnation, from_seq, until_incarnation, until_seq,
				source, precision, effective, established_at
			) VALUES (?, ?, 'GAP', ?, ?, ?, ?, 'AGENT_GAP', ?, 1, ?)
		`, archiveID, agentID, interval.From.Incarnation, interval.From.Seq,
			interval.Until.Incarnation, interval.Until.Seq,
			string(entry.precision), formatTime(now)); err != nil {
			return err
		}
	}
	return nil
}

func loadLatestClaims(ctx context.Context, tx *connectionTx, agentID string, revision uint64) ([]GapClaim, []uint64, error) {
	rows, err := tx.query(ctx, `
		SELECT claim_type, incarnation, from_seq, until_seq, reason, precision
		FROM agent_coverage_claims
		WHERE agent_id = ? AND coverage_revision = ?
		ORDER BY incarnation, from_seq
	`, agentID, revision)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var claims []GapClaim
	var unknown []uint64
	for rows.Next() {
		var kind, reason, precision string
		var incarnation int64
		var from, until sql.NullInt64
		if err := rows.Scan(&kind, &incarnation, &from, &until, &reason, &precision); err != nil {
			return nil, nil, err
		}
		if kind == "COVERAGE_UNKNOWN" {
			unknown = append(unknown, uint64(incarnation))
			continue
		}
		claims = append(claims, GapClaim{
			Incarnation: uint64(incarnation), FromSeq: uint64(from.Int64), UntilSeq: uint64(until.Int64),
			Reason: reason, Precision: Precision(precision),
		})
	}
	return claims, unknown, rows.Err()
}

func missingRanges(ctx context.Context, tx *connectionTx, agentID string, claim GapClaim) ([]Range, error) {
	rows, err := tx.query(ctx, `
		SELECT seq FROM audit_events
		WHERE agent_id = ? AND incarnation = ? AND seq >= ? AND seq < ?
		ORDER BY seq
	`, agentID, claim.Incarnation, claim.FromSeq, claim.UntilSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	next := claim.FromSeq
	var missing []Range
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		if seq > next {
			missing = append(missing, Range{From: Cursor{claim.Incarnation, next}, Until: Cursor{claim.Incarnation, seq}})
		}
		if seq >= next {
			next = seq + 1
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if next < claim.UntilSeq {
		missing = append(missing, Range{From: Cursor{claim.Incarnation, next}, Until: Cursor{claim.Incarnation, claim.UntilSeq}})
	}
	return missing, nil
}

func loadCurrentEffective(ctx context.Context, tx *connectionTx, archiveID, agentID string) ([]desiredCoverage, error) {
	rows, err := tx.query(ctx, `
		SELECT from_incarnation, from_seq, until_incarnation, until_seq, precision
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type = 'GAP' AND source = 'AGENT_GAP'
		  AND effective = 1 AND resolved_at IS NULL
	`, archiveID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []desiredCoverage
	for rows.Next() {
		var fromInc int64
		var fromSeq, untilInc, untilSeq sql.NullInt64
		var precision string
		if err := rows.Scan(&fromInc, &fromSeq, &untilInc, &untilSeq, &precision); err != nil {
			return nil, err
		}
		entry := desiredCoverage{precision: Precision(precision)}
		if !fromSeq.Valid {
			entry.unknownIncarnation = uint64(fromInc)
		} else {
			entry.rangeValue = &Range{
				From:  Cursor{uint64(fromInc), uint64(fromSeq.Int64)},
				Until: Cursor{uint64(untilInc.Int64), uint64(untilSeq.Int64)},
			}
		}
		result = append(result, entry)
	}
	sortDesired(result)
	return result, rows.Err()
}

func sortDesired(entries []desiredCoverage) {
	sort.Slice(entries, func(i, j int) bool {
		leftInc, leftSeq := entries[i].unknownIncarnation, uint64(0)
		rightInc, rightSeq := entries[j].unknownIncarnation, uint64(0)
		if entries[i].rangeValue != nil {
			leftInc, leftSeq = entries[i].rangeValue.From.Incarnation, entries[i].rangeValue.From.Seq
		}
		if entries[j].rangeValue != nil {
			rightInc, rightSeq = entries[j].rangeValue.From.Incarnation, entries[j].rangeValue.From.Seq
		}
		if leftInc != rightInc {
			return leftInc < rightInc
		}
		return leftSeq < rightSeq
	})
}

func equalDesired(left, right []desiredCoverage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].unknownIncarnation != right[index].unknownIncarnation || left[index].precision != right[index].precision {
			return false
		}
		if (left[index].rangeValue == nil) != (right[index].rangeValue == nil) {
			return false
		}
		if left[index].rangeValue != nil && *left[index].rangeValue != *right[index].rangeValue {
			return false
		}
	}
	return true
}

func (s *Store) EffectiveGaps(ctx context.Context, archiveID, agentID string) ([]EffectiveGap, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, from_incarnation, from_seq, until_incarnation, until_seq,
		       precision, source, established_at
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type IN ('GAP', 'REGRESSION') AND effective = 1 AND resolved_at IS NULL
		ORDER BY from_incarnation, from_seq
	`, archiveID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []EffectiveGap
	for rows.Next() {
		var item EffectiveGap
		var fromInc int64
		var fromSeq, untilInc, untilSeq sql.NullInt64
		var established string
		if err := rows.Scan(&item.ID, &fromInc, &fromSeq, &untilInc, &untilSeq,
			&item.Precision, &item.Source, &established); err != nil {
			return nil, err
		}
		item.Incarnation = uint64(fromInc)
		if fromSeq.Valid {
			item.Range = &Range{From: Cursor{uint64(fromInc), uint64(fromSeq.Int64)}, Until: Cursor{uint64(untilInc.Int64), uint64(untilSeq.Int64)}}
		}
		item.EstablishedAt, err = parseTime(established)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
