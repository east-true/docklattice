package serverstore

import (
	"context"
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
