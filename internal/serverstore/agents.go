package serverstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAgentNotFound = errors.New("serverstore: Agent not found")

const serverDatabaseTimeFormat = "2006-01-02T15:04:05.000000000Z"

// TouchAgentLastSeen monotonically advances the durable liveness observation.
// Retired Agents are immutable and cannot be revived by a delayed heartbeat.
func (s *Store) TouchAgentLastSeen(ctx context.Context, agentID string, observedAt time.Time) error {
	if s == nil || s.db == nil || agentID == "" || observedAt.IsZero() {
		return errors.New("serverstore: Agent ID and observation time are required")
	}
	canonical := observedAt.UTC().Format(serverDatabaseTimeFormat)
	result, err := s.db.ExecContext(ctx, `
		UPDATE agents
		SET last_seen_at = CASE WHEN last_seen_at < ? THEN ? ELSE last_seen_at END
		WHERE id = ? AND retired_at IS NULL
	`, canonical, canonical, agentID)
	if err != nil {
		return fmt.Errorf("serverstore: update Agent last_seen: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAgentNotFound
	}
	return nil
}

// ErrAgentRetired reports that an Agent row exists but has been retired. A
// retired Agent is immutable and is never revived by an authenticated session.
var ErrAgentRetired = errors.New("serverstore: Agent is retired")

// RestoreAuthenticatedAgent recreates the operational record of an Agent whose
// credential the transport has already verified. Section 6.1 of the
// architecture separates the two Server-side loss outcomes: losing the Audit /
// operational database while the Identity State survives must let existing
// Agents authenticate automatically, whereas losing the Identity State requires
// manual re-registration. The signing keys and the revocation ledger live in the
// Identity State, so after a database loss the credential still verifies while
// the agents row is gone. Restoring the row here is what makes that automatic
// reconnection observable through the Server API.
//
// The display name is operational data that was lost with the database, so the
// restored row carries the Agent ID until an operator re-registers the Agent
// under its name again.
func (s *Store) RestoreAuthenticatedAgent(ctx context.Context, agentID string, observedAt time.Time) error {
	if s == nil || s.db == nil || agentID == "" || observedAt.IsZero() {
		return errors.New("serverstore: Agent ID and observation time are required")
	}
	canonical := observedAt.UTC().Format(serverDatabaseTimeFormat)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at, metadata_json, capabilities_json)
		VALUES (?, ?, ?, ?, '{}', '{}')
		ON CONFLICT(id) DO NOTHING
	`, agentID, agentID, canonical, canonical)
	if err != nil {
		return fmt.Errorf("serverstore: restore authenticated Agent: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	var retiredAt sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT retired_at FROM agents WHERE id = ?", agentID).Scan(&retiredAt); err != nil {
		return fmt.Errorf("serverstore: inspect restored Agent: %w", err)
	}
	if retiredAt.Valid {
		return ErrAgentRetired
	}
	return nil
}
