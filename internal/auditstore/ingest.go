package auditstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func (s *Store) Ingest(
	ctx context.Context,
	archiveID, agentID string,
	events []Event,
	deliveryNext Cursor,
	now time.Time,
) (IngestResult, error) {
	if archiveID == "" || agentID == "" || !validCursor(deliveryNext) {
		return IngestResult{}, fmt.Errorf("%w: invalid ingest identity/cursor", ErrInvariant)
	}
	for index, event := range events {
		if event.AgentID != agentID || !validCursor(event.Cursor) || event.Kind == "" {
			return IngestResult{}, fmt.Errorf("%w: invalid audit event", ErrInvariant)
		}
		if index > 0 && compareCursor(events[index-1].Cursor, event.Cursor) >= 0 {
			return IngestResult{}, fmt.Errorf("%w: audit batch is not strictly ordered", ErrInvariant)
		}
		if compareCursor(event.Cursor, deliveryNext) >= 0 {
			return IngestResult{}, fmt.Errorf("%w: event is not before delivery next cursor", ErrInvariant)
		}
		if len(event.Metadata) == 0 {
			event.Metadata = json.RawMessage(`{}`)
		}
		if !json.Valid(event.Metadata) {
			return IngestResult{}, fmt.Errorf("%w: invalid event metadata JSON", ErrInvariant)
		}
	}
	result := IngestResult{DeliveryNext: deliveryNext}
	err := s.withImmediate(ctx, func(tx *connectionTx) error {
		state, err := loadCursorState(ctx, tx, archiveID, agentID)
		if err != nil {
			return err
		}
		if state.next != nil && compareCursor(deliveryNext, *state.next) < 0 {
			return ErrCursorRollback
		}
		for _, event := range events {
			metadata := event.Metadata
			if len(metadata) == 0 {
				metadata = json.RawMessage(`{}`)
			}
			execResult, err := tx.exec(ctx, `
				INSERT INTO audit_events(
					agent_id, incarnation, seq, occurred_at, kind, actor,
					project_uid, operation_id, resource_type, resource_id, action, metadata_json
				) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)
				ON CONFLICT(agent_id, incarnation, seq) DO NOTHING
			`, agentID, event.Cursor.Incarnation, event.Cursor.Seq,
				formatTime(event.OccurredAt), event.Kind, event.Actor,
				event.ProjectUID, event.OperationID, event.ResourceType, event.ResourceID, event.Action, string(metadata))
			if err != nil {
				return err
			}
			inserted, err := execResult.RowsAffected()
			if err != nil {
				return err
			}
			if inserted == 1 {
				result.Inserted++
			} else {
				if err := verifyDuplicate(ctx, tx, agentID, event, metadata); err != nil {
					return err
				}
				result.Duplicates++
			}
		}
		if _, err := tx.exec(ctx, `
			UPDATE agent_cursors SET next_incarnation = ?, next_seq = ?
			WHERE audit_archive_id = ? AND agent_id = ?
		`, deliveryNext.Incarnation, deliveryNext.Seq, archiveID, agentID); err != nil {
			return err
		}
		return recomputeEffective(ctx, tx, archiveID, agentID, now)
	})
	if err != nil {
		return IngestResult{}, err
	}
	if result.Inserted > 0 {
		s.mu.Lock()
		state := s.runtimeFor(archiveID, agentID)
		if !state.ingestedSinceACK {
			state.firstUnackedIngestAt = now.UTC()
		}
		state.lastIngestAt = now.UTC()
		state.ingestedSinceACK = true
		s.mu.Unlock()
	}
	return result, nil
}

func verifyDuplicate(ctx context.Context, tx *connectionTx, agentID string, event Event, metadata json.RawMessage) error {
	var occurredAt, kind, resourceType, resourceID, action, metadataJSON string
	var actor, projectUID, operationID sql.NullString
	err := tx.row(ctx, `
		SELECT occurred_at, kind, actor, project_uid, operation_id, resource_type, resource_id, action, metadata_json
		FROM audit_events
		WHERE agent_id = ? AND incarnation = ? AND seq = ?
	`, agentID, event.Cursor.Incarnation, event.Cursor.Seq).Scan(
		&occurredAt, &kind, &actor, &projectUID, &operationID, &resourceType, &resourceID, &action, &metadataJSON,
	)
	if err != nil {
		return err
	}
	if occurredAt != formatTime(event.OccurredAt) || kind != event.Kind ||
		actor.String != event.Actor || actor.Valid != (event.Actor != "") ||
		projectUID.String != event.ProjectUID || projectUID.Valid != (event.ProjectUID != "") ||
		operationID.String != event.OperationID || operationID.Valid != (event.OperationID != "") ||
		resourceType != event.ResourceType || resourceID != event.ResourceID || action != event.Action ||
		!bytes.Equal([]byte(metadataJSON), metadata) {
		return fmt.Errorf("%w: duplicate audit cursor has different content", ErrInvariant)
	}
	return nil
}
