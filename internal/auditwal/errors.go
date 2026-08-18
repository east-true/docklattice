package auditwal

import (
	"errors"
	"fmt"
)

var (
	ErrClosed          = errors.New("AUDIT_WAL_CLOSED")
	ErrInvariant       = errors.New("AUDIT_WAL_INVARIANT_VIOLATION")
	ErrCorrupt         = errors.New("AUDIT_WAL_CORRUPT")
	ErrRecordTooLarge  = errors.New("AUDIT_WAL_RECORD_TOO_LARGE")
	ErrCursorRollback  = errors.New("AUDIT_ACK_CURSOR_ROLLBACK")
	ErrCursorAhead     = errors.New("AUDIT_ACK_CURSOR_AHEAD")
	ErrArchiveMismatch = errors.New("AUDIT_ARCHIVE_IDENTITY_MISMATCH")
	ErrStaleCoverage   = errors.New("STALE_COVERAGE")
)

type StaleCoverageError struct {
	SeenRevision                uint64
	CurrentRevision             uint64
	CurrentACK                  *Cursor
	BlockingRanges              []Gap
	BlockingUnknownIncarnations []uint64
	Coverage                    CoverageSnapshot
}

func (e *StaleCoverageError) Error() string {
	return fmt.Sprintf("%s: server saw revision %d, current revision %d, %d blocking ranges",
		ErrStaleCoverage, e.SeenRevision, e.CurrentRevision, len(e.BlockingRanges)+len(e.BlockingUnknownIncarnations))
}
func (e *StaleCoverageError) Unwrap() error { return ErrStaleCoverage }
