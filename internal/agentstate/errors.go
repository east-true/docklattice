package agentstate

import (
	"errors"
	"fmt"
)

var (
	ErrArchiveRollbackDetected = errors.New("ARCHIVE_ROLLBACK_DETECTED")
	ErrArchiveInvariant        = errors.New("AUDIT_ARCHIVE_IDENTITY_INVARIANT_VIOLATION")
	ErrServerIdentityMismatch  = errors.New("SERVER_IDENTITY_MISMATCH")
	ErrCursorRollback          = errors.New("ARCHIVE_ACK_CURSOR_ROLLBACK")
	ErrStateInvariant          = errors.New("AGENT_STATE_INVARIANT_VIOLATION")
	ErrSymlink                 = errors.New("AGENT_STATE_SYMLINK_REJECTED")
	ErrInsecureMode            = errors.New("AGENT_STATE_INSECURE_MODE")
	ErrStateLocked             = errors.New("AGENT_STATE_LOCKED")
	ErrNotReady                = errors.New("AGENT_STATE_NOT_READY")
	ErrClosed                  = errors.New("AGENT_STATE_CLOSED")
)

// ArchiveRollbackError reports a Server archive generation rollback.
type ArchiveRollbackError struct {
	BoundGeneration     uint64
	PresentedGeneration uint64
}

func (e *ArchiveRollbackError) Error() string {
	return fmt.Sprintf("%s: bound generation %d, presented generation %d",
		ErrArchiveRollbackDetected, e.BoundGeneration, e.PresentedGeneration)
}

func (e *ArchiveRollbackError) Unwrap() error { return ErrArchiveRollbackDetected }

// ArchiveInvariantError reports an impossible identity/generation/archive tuple.
type ArchiveInvariantError struct {
	Generation         uint64
	BoundArchiveID     string
	PresentedArchiveID string
	Reason             string
}

func (e *ArchiveInvariantError) Error() string {
	return fmt.Sprintf("%s: generation %d, bound archive %q, presented archive %q: %s",
		ErrArchiveInvariant, e.Generation, e.BoundArchiveID, e.PresentedArchiveID, e.Reason)
}

func (e *ArchiveInvariantError) Unwrap() error { return ErrArchiveInvariant }

// StateInvariantError reports persistent state that cannot be reconciled with
// the caller's durable WAL facts.
type StateInvariantError struct {
	Reason string
}

func (e *StateInvariantError) Error() string {
	return fmt.Sprintf("%s: %s", ErrStateInvariant, e.Reason)
}

func (e *StateInvariantError) Unwrap() error { return ErrStateInvariant }
