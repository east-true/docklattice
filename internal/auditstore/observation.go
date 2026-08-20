package auditstore

import (
	"context"
	"fmt"
	"time"
)

const ackBlockedThreshold = 5 * time.Minute

// Observe returns a read-consistent view of the persisted cursors, canonical
// records, and Coverage Ledger. Process-lifetime counters are merged only
// after the SQLite snapshot completes.
func (s *Store) Observe(
	ctx context.Context,
	archiveID, agentID string,
	agentOnline bool,
	coverageRevisionCurrent uint64,
	now time.Time,
) (Observation, error) {
	if archiveID == "" || agentID == "" {
		return Observation{}, fmt.Errorf("%w: invalid observation identity", ErrInvariant)
	}
	var observation Observation
	var ackUpdatedAt time.Time
	err := s.withImmediate(ctx, func(tx *connectionTx) error {
		state, err := loadCursorState(ctx, tx, archiveID, agentID)
		if err != nil {
			return err
		}
		observation.ACKCursor = cloneCursor(state.ack)
		observation.CoverageRevisionSeen = state.revision
		observation.CoverageRevisionCurrent = coverageRevisionCurrent
		ackUpdatedAt = state.ackUpdatedAt

		if state.ack == nil {
			err = tx.row(ctx, `
				SELECT COUNT(*), COALESCE(SUM(
					length(agent_id) + length(kind) + length(occurred_at) +
					length(COALESCE(actor, '')) + length(COALESCE(project_uid, '')) +
					length(COALESCE(operation_id, '')) + length(metadata_json) + 16
				), 0)
				FROM audit_events WHERE agent_id = ?
			`, agentID).Scan(&observation.IngestedUnackedRecords, &observation.IngestedUnackedBytes)
		} else {
			err = tx.row(ctx, `
				SELECT COUNT(*), COALESCE(SUM(
					length(agent_id) + length(kind) + length(occurred_at) +
					length(COALESCE(actor, '')) + length(COALESCE(project_uid, '')) +
					length(COALESCE(operation_id, '')) + length(metadata_json) + 16
				), 0)
				FROM audit_events
				WHERE agent_id = ? AND (
					incarnation > ? OR (incarnation = ? AND seq > ?)
				)
			`, agentID, state.ack.Incarnation, state.ack.Incarnation, state.ack.Seq).
				Scan(&observation.IngestedUnackedRecords, &observation.IngestedUnackedBytes)
		}
		if err != nil {
			return err
		}

		if err := tx.row(ctx, `
			SELECT COALESCE(SUM(until_seq - from_seq), 0)
			FROM server_archive_coverage
			WHERE audit_archive_id = ? AND agent_id = ?
			  AND entry_type IN ('GAP', 'REGRESSION') AND effective = 1 AND resolved_at IS NULL
			  AND from_seq IS NOT NULL AND until_seq IS NOT NULL
		`, archiveID, agentID).Scan(&observation.EffectiveGapRecords); err != nil {
			return err
		}
		return tx.row(ctx, `
			SELECT COUNT(*) FROM agent_coverage_claims
			WHERE agent_id = ? AND claim_type = 'GAP'
		`, agentID).Scan(&observation.AgentGapClaimsTotal)
	})
	if err != nil {
		return Observation{}, err
	}

	s.mu.Lock()
	runtime := s.runtimeFor(archiveID, agentID)
	observation.StaleCoverageTotal = runtime.staleCoverageTotal
	observation.ACKRetryTotal = runtime.ackRetryTotal
	activelyStalled := agentOnline && runtime.ingestedSinceACK && observation.IngestedUnackedRecords > 0
	stalledSince := ackUpdatedAt
	if runtime.firstUnackedIngestAt.After(stalledSince) {
		stalledSince = runtime.firstUnackedIngestAt
	}
	if activelyStalled && now.After(stalledSince) {
		observation.ACKWatermarkStalled = now.Sub(stalledSince)
	}
	if activelyStalled && observation.ACKWatermarkStalled >= ackBlockedThreshold {
		observation.ACKBlockedWhileIngesting = true
		if runtime.blockedSince.IsZero() {
			runtime.blockedSince = stalledSince.Add(ackBlockedThreshold)
		}
		if now.After(runtime.blockedSince) {
			observation.ACKBlockedWhileIngestingFor = now.Sub(runtime.blockedSince)
		}
	} else {
		runtime.blockedSince = time.Time{}
	}
	s.mu.Unlock()
	return observation, nil
}
