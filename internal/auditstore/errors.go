package auditstore

import (
	"errors"
	"fmt"
)

var (
	ErrInvariant        = errors.New("AUDIT_COVERAGE_INVARIANT_VIOLATION")
	ErrStaleClaim       = errors.New("STALE_COVERAGE_CLAIM")
	ErrCursorRollback   = errors.New("SERVER_ACK_CURSOR_ROLLBACK")
	ErrACKIneligible    = errors.New("AUDIT_ACK_INELIGIBLE")
	ErrCoverageRevision = errors.New("COVERAGE_REVISION_MISMATCH")
)

type StaleClaimError struct {
	Presented uint64
	Current   uint64
}

func (e *StaleClaimError) Error() string {
	return fmt.Sprintf("%s: presented revision %d, current revision %d", ErrStaleClaim, e.Presented, e.Current)
}
func (e *StaleClaimError) Unwrap() error { return ErrStaleClaim }

type ACKIneligibleError struct {
	Proposed     Cursor
	DeliveryNext Cursor
	Unexplained  []Range
}

func (e *ACKIneligibleError) Error() string {
	return fmt.Sprintf("%s: proposed (%d,%d), %d unexplained ranges",
		ErrACKIneligible, e.Proposed.Incarnation, e.Proposed.Seq, len(e.Unexplained))
}
func (e *ACKIneligibleError) Unwrap() error { return ErrACKIneligible }
