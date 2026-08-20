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

// Error names the cursors an operator needs to tell the two recoverable shapes
// apart: a restored archive whose delivery cursor sits far below what the Agent
// is offering, and an ordinary hole in what is arriving now. Cursors are
// positions, not content, so none of this discloses Audit payload.
func (e *ACKIneligibleError) Error() string {
	if len(e.Unexplained) == 0 {
		return fmt.Sprintf("%s: proposed (%d,%d), delivery next (%d,%d)",
			ErrACKIneligible, e.Proposed.Incarnation, e.Proposed.Seq,
			e.DeliveryNext.Incarnation, e.DeliveryNext.Seq)
	}
	first := e.Unexplained[0]
	return fmt.Sprintf("%s: proposed (%d,%d), delivery next (%d,%d), %d unexplained ranges from [(%d,%d),(%d,%d))",
		ErrACKIneligible, e.Proposed.Incarnation, e.Proposed.Seq,
		e.DeliveryNext.Incarnation, e.DeliveryNext.Seq, len(e.Unexplained),
		first.From.Incarnation, first.From.Seq, first.Until.Incarnation, first.Until.Seq)
}
func (e *ACKIneligibleError) Unwrap() error { return ErrACKIneligible }
