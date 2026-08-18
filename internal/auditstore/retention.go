package auditstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const retentionBatchRows = 512

const (
	currentArchiveSettingKey = "audit_archive_identity"
	retentionSource          = "SERVER_RETENTION"
	retentionAppliedReason   = "SERVER_RETENTION_APPLIED"
	quotaBeforeACKReason     = "QUOTA_PRESSURE_BEFORE_AGENT_ACK"
)

type DefaultRetentionPolicy struct {
	MaxAge            time.Duration
	MaxBytes          int64
	WarningPercent    int
	AggressivePercent int
	LowPercent        int
}

func NewDefaultRetentionPolicy() DefaultRetentionPolicy {
	return DefaultRetentionPolicy{
		MaxAge: DefaultServerAuditRetention, MaxBytes: DefaultServerAuditMaxBytes,
		WarningPercent: DefaultWarningPercent, AggressivePercent: DefaultAggressivePercent,
		LowPercent: DefaultLowWatermarkPercent,
	}
}

func (policy DefaultRetentionPolicy) Plan(_ context.Context, usage ArchiveUsage) (RetentionPlan, error) {
	if policy.MaxAge <= 0 || policy.MaxBytes <= 0 || policy.WarningPercent <= 0 ||
		policy.WarningPercent >= policy.AggressivePercent || policy.AggressivePercent >= 100 ||
		policy.LowPercent <= 0 || policy.LowPercent > policy.WarningPercent || usage.EvaluatedAt.IsZero() || usage.ApproximateBytes < 0 {
		return RetentionPlan{}, fmt.Errorf("%w: invalid retention policy or usage", ErrInvariant)
	}
	warningBytes := percentage(policy.MaxBytes, policy.WarningPercent)
	aggressiveBytes := percentage(policy.MaxBytes, policy.AggressivePercent)
	plan := RetentionPlan{
		DeleteBefore: usage.EvaluatedAt.UTC().Add(-policy.MaxAge),
		Warning:      usage.ApproximateBytes >= warningBytes,
		Aggressive:   usage.ApproximateBytes >= aggressiveBytes,
	}
	if plan.Aggressive {
		plan.PressureTargetBytes = percentage(policy.MaxBytes, policy.LowPercent)
	}
	return plan, nil
}

func percentage(value int64, percent int) int64 {
	return value/100*int64(percent) + value%100*int64(percent)/100
}

// EnforceRetention never gates Ingest. Each bounded deletion batch uses its
// own IMMEDIATE transaction so Coverage Ledger publication and record deletion
// are atomic while other writers can proceed between batches.
func (s *Store) EnforceRetention(ctx context.Context, archiveID string, policy RetentionPolicy, now time.Time) (RetentionResult, error) {
	if archiveID == "" || now.IsZero() {
		return RetentionResult{}, fmt.Errorf("%w: invalid retention request", ErrInvariant)
	}
	// audit_events are intentionally canonical and keyed globally by
	// (agent, incarnation, seq), not duplicated per Archive. Consequently this
	// executor may only mutate the operational DB's current canonical Archive.
	if err := s.requireCurrentArchive(ctx, archiveID); err != nil {
		return RetentionResult{}, err
	}
	if policy == nil {
		defaultPolicy := NewDefaultRetentionPolicy()
		policy = defaultPolicy
	}
	before, err := s.ArchiveUsage(ctx, archiveID, now)
	if err != nil {
		return RetentionResult{}, err
	}
	plan, err := policy.Plan(ctx, before)
	if err != nil {
		return RetentionResult{}, err
	}
	if plan.PressureTargetBytes < 0 || !plan.DeleteBefore.IsZero() && plan.DeleteBefore.After(now) ||
		!plan.Aggressive && plan.PressureTargetBytes != 0 ||
		plan.Aggressive && (!plan.Warning || plan.PressureTargetBytes >= before.ApproximateBytes) {
		return RetentionResult{}, fmt.Errorf("%w: invalid retention plan", ErrInvariant)
	}
	result := RetentionResult{UsageBefore: before, Level: RetentionNormal}
	if plan.Warning {
		result.Level = RetentionWarning
	}
	if plan.Aggressive {
		result.Level = RetentionAggressive
	}

	if err := s.deleteEventTier(ctx, archiveID, plan, now, true, false, retentionAppliedReason, &result); err != nil {
		return result, err
	}
	if err := s.compactCoverageHistory(ctx, archiveID, plan, now, &result); err != nil {
		return result, err
	}
	if err := s.deleteEventTier(ctx, archiveID, plan, now, false, true, "", &result); err != nil {
		return result, err
	}
	if err := s.deleteEventTier(ctx, archiveID, plan, now, false, false, quotaBeforeACKReason, &result); err != nil {
		return result, err
	}
	after, err := s.ArchiveUsage(ctx, archiveID, now)
	if err != nil {
		return result, err
	}
	result.UsageAfter = after
	result.LowWatermarkReached = !plan.Aggressive || after.ApproximateBytes <= plan.PressureTargetBytes
	return result, nil
}

func (s *Store) requireCurrentArchive(ctx context.Context, archiveID string) error {
	var payload string
	if err := s.db.QueryRowContext(ctx, `
		SELECT value_json FROM settings WHERE key = ?
	`, currentArchiveSettingKey).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: current canonical Audit Archive is not initialized", ErrInvariant)
		}
		return err
	}
	return validateCurrentArchivePayload(payload, archiveID)
}

func requireCurrentArchiveTx(ctx context.Context, tx *connectionTx, archiveID string) error {
	var payload string
	if err := tx.row(ctx, `SELECT value_json FROM settings WHERE key = ?`, currentArchiveSettingKey).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: current canonical Audit Archive is not initialized", ErrInvariant)
		}
		return err
	}
	return validateCurrentArchivePayload(payload, archiveID)
}

func validateCurrentArchivePayload(payload, archiveID string) error {
	var identity struct {
		ServerIdentityID string `json:"server_identity_id"`
		Generation       uint64 `json:"archive_generation"`
		AuditArchiveID   string `json:"audit_archive_id"`
	}
	if err := json.Unmarshal([]byte(payload), &identity); err != nil || identity.ServerIdentityID == "" ||
		identity.Generation == 0 || identity.AuditArchiveID == "" {
		return fmt.Errorf("%w: invalid current canonical Audit Archive identity", ErrInvariant)
	}
	if identity.AuditArchiveID != archiveID {
		return fmt.Errorf("%w: retention target is not the current canonical Audit Archive", ErrInvariant)
	}
	return nil
}

func (s *Store) ArchiveUsage(ctx context.Context, archiveID string, now time.Time) (ArchiveUsage, error) {
	if archiveID == "" || now.IsZero() {
		return ArchiveUsage{}, fmt.Errorf("%w: invalid archive usage request", ErrInvariant)
	}
	usage := ArchiveUsage{AuditArchiveID: archiveID, EvaluatedAt: now.UTC()}
	var oldest sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(
			length(e.agent_id) + length(e.kind) + length(e.occurred_at) +
			length(COALESCE(e.actor, '')) + length(COALESCE(e.project_uid, '')) +
			length(COALESCE(e.operation_id, '')) + length(e.metadata_json) + 32
		), 0)
		FROM audit_events e
		WHERE EXISTS (
			SELECT 1 FROM agent_cursors c
			WHERE c.audit_archive_id = ? AND c.agent_id = e.agent_id
		)
	`, archiveID).Scan(&usage.RecordCount, &usage.ApproximateBytes)
	if err != nil {
		return ArchiveUsage{}, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT e.occurred_at FROM audit_events e
		WHERE EXISTS (
			SELECT 1 FROM agent_cursors c
			WHERE c.audit_archive_id = ? AND c.agent_id = e.agent_id
		)
		ORDER BY julianday(e.occurred_at), e.id LIMIT 1
	`, archiveID).Scan(&oldest)
	if err != nil && err != sql.ErrNoRows {
		return ArchiveUsage{}, err
	}
	if oldest.Valid {
		usage.OldestOccurredAt, err = parseTime(oldest.String)
		if err != nil {
			return ArchiveUsage{}, err
		}
	}
	var claimsBytes, coverageBytes int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			length(claim_type) + length(reason) + length(precision) + length(reported_at) + 48
		), 0)
		FROM agent_coverage_claims claims
		WHERE EXISTS (
			SELECT 1 FROM agent_cursors c
			WHERE c.audit_archive_id = ? AND c.agent_id = claims.agent_id
		)
	`, archiveID).Scan(&claimsBytes); err != nil {
		return ArchiveUsage{}, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			length(audit_archive_id) + length(agent_id) + length(entry_type) +
			length(source) + length(precision) + length(established_at) +
			length(COALESCE(resolved_at, '')) + length(COALESCE(reason, '')) + 64
		), 0)
		FROM server_archive_coverage WHERE audit_archive_id = ?
	`, archiveID).Scan(&coverageBytes); err != nil {
		return ArchiveUsage{}, err
	}
	usage.ApproximateBytes += claimsBytes + coverageBytes
	return usage, nil
}

type retentionEvent struct {
	id          int64
	agentID     string
	cursor      Cursor
	occurredAt  time.Time
	approxBytes int64
}

func (s *Store) deleteEventTier(ctx context.Context, archiveID string, plan RetentionPlan, now time.Time, acked, alreadyCovered bool, reason string, result *RetentionResult) error {
	for {
		usage, err := s.ArchiveUsage(ctx, archiveID, now)
		if err != nil {
			return err
		}
		pressure := plan.Aggressive && usage.ApproximateBytes > plan.PressureTargetBytes
		if !pressure && plan.DeleteBefore.IsZero() {
			return nil
		}
		var selected []retentionEvent
		var createdIntervals int64
		err = s.withImmediate(ctx, func(tx *connectionTx) error {
			if err := requireCurrentArchiveTx(ctx, tx, archiveID); err != nil {
				return err
			}
			candidates, err := loadRetentionEvents(ctx, tx, archiveID, plan.DeleteBefore, pressure, acked, alreadyCovered)
			if err != nil {
				return err
			}
			remaining := usage.ApproximateBytes
			for _, candidate := range candidates {
				expired := !plan.DeleteBefore.IsZero() && candidate.occurredAt.Before(plan.DeleteBefore)
				if !expired && (!pressure || remaining <= plan.PressureTargetBytes) {
					break
				}
				selected = append(selected, candidate)
				remaining -= candidate.approxBytes
			}
			if len(selected) == 0 {
				return nil
			}
			if reason != "" {
				intervals := retentionIntervals(selected)
				for _, interval := range intervals {
					if err := recordRetentionInterval(ctx, tx, archiveID, interval.agentID, interval.value, reason, now); err != nil {
						return err
					}
				}
				createdIntervals = int64(len(intervals))
			}
			for _, candidate := range selected {
				if _, err := tx.exec(ctx, `DELETE FROM audit_events WHERE id = ?`, candidate.id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			return nil
		}
		result.CreatedCoverageIntervals += createdIntervals
		switch {
		case alreadyCovered:
			result.DeletedCoveredRecords += int64(len(selected))
		case acked:
			result.DeletedACKedRecords += int64(len(selected))
		default:
			result.DeletedUnACKedRecords += int64(len(selected))
		}
	}
}

func loadRetentionEvents(ctx context.Context, tx *connectionTx, archiveID string, cutoff time.Time, pressure, acked, alreadyCovered bool) ([]retentionEvent, error) {
	condition := `(c.acked_incarnation IS NOT NULL AND (
		e.incarnation < c.acked_incarnation OR
		(e.incarnation = c.acked_incarnation AND e.seq <= c.acked_seq)
	))`
	if !acked {
		condition = `NOT ` + condition
	}
	coveredFilter := `AND NOT EXISTS (
		SELECT 1 FROM server_archive_coverage coverage
		WHERE coverage.audit_archive_id = c.audit_archive_id
		  AND coverage.agent_id = e.agent_id
		  AND coverage.entry_type = 'GAP' AND coverage.source = 'SERVER_RETENTION'
		  AND coverage.effective = 1 AND coverage.resolved_at IS NULL
		  AND coverage.from_incarnation = e.incarnation
		  AND coverage.until_incarnation = e.incarnation
		  AND coverage.from_seq <= e.seq AND coverage.until_seq > e.seq
	)`
	if alreadyCovered {
		condition = "1 = 1"
		coveredFilter = `AND EXISTS (
			SELECT 1 FROM server_archive_coverage coverage
			WHERE coverage.audit_archive_id = c.audit_archive_id
			  AND coverage.agent_id = e.agent_id
			  AND coverage.entry_type = 'GAP' AND coverage.source = 'SERVER_RETENTION'
			  AND coverage.effective = 1 AND coverage.resolved_at IS NULL
			  AND coverage.from_incarnation = e.incarnation
			  AND coverage.until_incarnation = e.incarnation
			  AND coverage.from_seq <= e.seq AND coverage.until_seq > e.seq
		)`
	}
	if cutoff.IsZero() && !pressure {
		return nil, nil
	}
	cutoffValue := ""
	if !cutoff.IsZero() {
		cutoffValue = formatTime(cutoff)
	}
	query := fmt.Sprintf(`
		SELECT e.id, e.agent_id, e.incarnation, e.seq, e.occurred_at,
		       length(e.agent_id) + length(e.kind) + length(e.occurred_at) +
		       length(COALESCE(e.actor, '')) + length(COALESCE(e.project_uid, '')) +
		       length(COALESCE(e.operation_id, '')) + length(e.metadata_json) + 32
		FROM audit_events e
		JOIN agent_cursors c ON c.agent_id = e.agent_id AND c.audit_archive_id = ?
		WHERE %s %s AND (? = 1 OR julianday(e.occurred_at) < julianday(?))
		ORDER BY julianday(e.occurred_at), e.agent_id, e.incarnation, e.seq
		LIMIT %d
	`, condition, coveredFilter, retentionBatchRows)
	rows, err := tx.query(ctx, query, archiveID, boolInteger(pressure), cutoffValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []retentionEvent
	for rows.Next() {
		var event retentionEvent
		var incarnation, seq int64
		var occurred string
		if err := rows.Scan(&event.id, &event.agentID, &incarnation, &seq, &occurred, &event.approxBytes); err != nil {
			return nil, err
		}
		event.cursor = Cursor{uint64(incarnation), uint64(seq)}
		event.occurredAt, err = parseTime(occurred)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

type agentRange struct {
	agentID string
	value   Range
}

func retentionIntervals(events []retentionEvent) []agentRange {
	ordered := append([]retentionEvent(nil), events...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].agentID != ordered[j].agentID {
			return ordered[i].agentID < ordered[j].agentID
		}
		return compareCursor(ordered[i].cursor, ordered[j].cursor) < 0
	})
	result := make([]agentRange, 0)
	for _, event := range ordered {
		last := len(result) - 1
		if last >= 0 && result[last].agentID == event.agentID &&
			result[last].value.Until.Incarnation == event.cursor.Incarnation && result[last].value.Until.Seq == event.cursor.Seq {
			result[last].value.Until.Seq++
			continue
		}
		result = append(result, agentRange{agentID: event.agentID, value: Range{
			From: event.cursor, Until: Cursor{Incarnation: event.cursor.Incarnation, Seq: event.cursor.Seq + 1},
		}})
	}
	return result
}

func recordRetentionInterval(ctx context.Context, tx *connectionTx, archiveID, agentID string, interval Range, reason string, now time.Time) error {
	rows, err := tx.query(ctx, `
		SELECT id, from_seq, until_seq FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type = 'GAP' AND source = ? AND reason = ?
		  AND precision = 'exact' AND effective = 1 AND resolved_at IS NULL
		  AND from_incarnation = ? AND until_incarnation = ?
		  AND until_seq >= ? AND from_seq <= ?
	`, archiveID, agentID, retentionSource, reason, interval.From.Incarnation,
		interval.From.Incarnation, interval.From.Seq, interval.Until.Seq)
	if err != nil {
		return err
	}
	var ids []int64
	from, until := interval.From.Seq, interval.Until.Seq
	for rows.Next() {
		var id, storedFrom, storedUntil int64
		if err := rows.Scan(&id, &storedFrom, &storedUntil); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		if uint64(storedFrom) < from {
			from = uint64(storedFrom)
		}
		if uint64(storedUntil) > until {
			until = uint64(storedUntil)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.exec(ctx, `UPDATE server_archive_coverage SET effective = 0, resolved_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
			return err
		}
	}
	_, err = tx.exec(ctx, `
		INSERT INTO server_archive_coverage(
			audit_archive_id, agent_id, entry_type,
			from_incarnation, from_seq, until_incarnation, until_seq,
			source, precision, effective, established_at, reason
		) VALUES (?, ?, 'GAP', ?, ?, ?, ?, ?, 'exact', 1, ?, ?)
	`, archiveID, agentID, interval.From.Incarnation, from, interval.From.Incarnation, until,
		retentionSource, formatTime(now), reason)
	return err
}

func (s *Store) compactCoverageHistory(ctx context.Context, archiveID string, plan RetentionPlan, now time.Time, result *RetentionResult) error {
	for _, table := range []string{"agent_coverage_claims", "server_archive_coverage"} {
		for {
			usage, err := s.ArchiveUsage(ctx, archiveID, now)
			if err != nil {
				return err
			}
			pressure := plan.Aggressive && usage.ApproximateBytes > plan.PressureTargetBytes
			if !pressure && plan.DeleteBefore.IsZero() {
				break
			}
			var deleted int64
			err = s.withImmediate(ctx, func(tx *connectionTx) error {
				if err := requireCurrentArchiveTx(ctx, tx, archiveID); err != nil {
					return err
				}
				candidates, err := loadCoverageHistory(ctx, tx, archiveID, table, plan.DeleteBefore, pressure)
				if err != nil {
					return err
				}
				remaining := usage.ApproximateBytes
				for _, candidate := range candidates {
					expired := !plan.DeleteBefore.IsZero() && candidate.recordedAt.Before(plan.DeleteBefore)
					if !expired && (!pressure || remaining <= plan.PressureTargetBytes) {
						break
					}
					if _, err := tx.exec(ctx, `DELETE FROM `+table+` WHERE id = ?`, candidate.id); err != nil {
						return err
					}
					remaining -= candidate.approxBytes
					deleted++
				}
				return nil
			})
			if err != nil {
				return err
			}
			result.CompactedCoverageHistoryRows += deleted
			if deleted == 0 {
				break
			}
		}
	}
	return nil
}

type retentionHistory struct {
	id          int64
	recordedAt  time.Time
	approxBytes int64
}

func loadCoverageHistory(ctx context.Context, tx *connectionTx, archiveID, table string, cutoff time.Time, pressure bool) ([]retentionHistory, error) {
	var query string
	if table == "agent_coverage_claims" {
		query = `
			SELECT claims.id, claims.reported_at,
			       length(claims.claim_type) + length(claims.reason) +
			       length(claims.precision) + length(claims.reported_at) + 48
			FROM agent_coverage_claims claims
			JOIN agent_cursors c ON c.audit_archive_id = ? AND c.agent_id = claims.agent_id
			WHERE c.acked_incarnation IS NOT NULL
			  AND claims.coverage_revision < (SELECT MAX(newest.coverage_revision) FROM agent_coverage_claims newest WHERE newest.agent_id = claims.agent_id)
			  AND (
				(claims.claim_type = 'GAP' AND (
					claims.incarnation < c.acked_incarnation OR
					(claims.incarnation = c.acked_incarnation AND claims.until_seq <= c.acked_seq + 1)
				)) OR
				(claims.claim_type = 'COVERAGE_UNKNOWN' AND claims.incarnation < c.acked_incarnation)
			  )
			  AND (? = 1 OR julianday(claims.reported_at) < julianday(?))
			ORDER BY julianday(claims.reported_at), claims.id LIMIT ?`
	} else if table == "server_archive_coverage" {
		query = `
			SELECT coverage.id, coverage.resolved_at,
			       length(coverage.audit_archive_id) + length(coverage.agent_id) +
			       length(coverage.entry_type) + length(coverage.source) +
			       length(coverage.precision) + length(coverage.established_at) +
			       length(coverage.resolved_at) + length(COALESCE(coverage.reason, '')) + 64
			FROM server_archive_coverage coverage
			JOIN agent_cursors c
			  ON c.audit_archive_id = coverage.audit_archive_id AND c.agent_id = coverage.agent_id
			WHERE coverage.audit_archive_id = ? AND coverage.resolved_at IS NOT NULL
			  AND coverage.entry_type = 'GAP' AND coverage.until_incarnation IS NOT NULL
			  AND c.acked_incarnation IS NOT NULL
			  AND (
				coverage.until_incarnation < c.acked_incarnation OR
				(coverage.until_incarnation = c.acked_incarnation AND coverage.until_seq <= c.acked_seq + 1)
			  )
			  AND (? = 1 OR julianday(coverage.resolved_at) < julianday(?))
			ORDER BY julianday(coverage.resolved_at), coverage.id LIMIT ?`
	} else {
		return nil, fmt.Errorf("%w: invalid retention table", ErrInvariant)
	}
	cutoffValue := ""
	if !cutoff.IsZero() {
		cutoffValue = formatTime(cutoff)
	}
	rows, err := tx.query(ctx, query, archiveID, boolInteger(pressure), cutoffValue, retentionBatchRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []retentionHistory
	for rows.Next() {
		var candidate retentionHistory
		var recordedAt string
		if err := rows.Scan(&candidate.id, &recordedAt, &candidate.approxBytes); err != nil {
			return nil, err
		}
		candidate.recordedAt, err = parseTime(recordedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func boolInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}
