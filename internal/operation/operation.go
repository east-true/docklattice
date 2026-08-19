package operation

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Operation is safe for concurrent progress, cancel, commit, and output calls.
type Operation struct {
	mu      sync.Mutex
	auditMu sync.Mutex

	engine *Engine
	spec   Spec
	record Record
	lease  *Lease

	outputLimit      int
	terminalNotified bool
	executionCancel  context.CancelCauseFunc
}

func (o *Operation) attachExecutionCancel(cancel context.CancelCauseFunc) {
	o.mu.Lock()
	shouldCancel := o.record.Status.Terminal() || !o.record.CancelRequestedAt.IsZero()
	if !shouldCancel {
		o.executionCancel = cancel
	}
	o.mu.Unlock()
	if shouldCancel {
		cancel(context.Canceled)
	}
}

func newOperation(engine *Engine, spec Spec, mode CancelMode, now time.Time, outputLimit int) *Operation {
	return &Operation{
		engine: engine,
		spec:   spec,
		record: Record{
			OperationID:    spec.OperationID,
			ProjectKey:     spec.ProjectKey,
			Target:         spec.Target,
			Type:           spec.Type,
			PayloadHash:    spec.PayloadHash,
			RequestedAt:    now,
			Status:         StatusRequested,
			Phase:          PhasePreparing,
			CancelMode:     mode,
			Revision:       1,
			LastProgressAt: now,
		},
		outputLimit: outputLimit,
	}
}

func (o *Operation) attachLease(lease *Lease) {
	o.mu.Lock()
	if o.record.Status.Terminal() {
		o.mu.Unlock()
		lease.Release()
		return
	}
	o.lease = lease
	o.mu.Unlock()
}

func (o *Operation) Snapshot() Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.record
	result.OutputTail = append([]byte(nil), o.record.OutputTail...)
	result.StalledWarning = result.Status == StatusRunning && result.CancelMode == CancelBeforeCommit && result.Phase == PhaseCommitting &&
		o.engine.config.Clock.Now().Sub(result.LastProgressAt) >= o.engine.config.StalledAfter
	return result
}

func (o *Operation) isTerminal() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.record.Status.Terminal()
}

func (o *Operation) auditDelivery() ManagedAuditDelivery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.record.ManagedAuditDelivery
}

// TransitionStatus applies the legal status graph. Success is accepted only
// after FINALIZING; failure/interruption may terminate earlier.
func (o *Operation) TransitionStatus(next Status, result, errorText string) error {
	o.mu.Lock()
	if !legalStatusTransition(o.record.Status, next) {
		current := o.record.Status
		o.mu.Unlock()
		return &Error{Code: CodeIllegalTransition, Message: fmt.Sprintf("status %s cannot transition to %s", current, next)}
	}
	if next == StatusSuccess && o.record.Phase != PhaseFinalizing {
		o.mu.Unlock()
		return &Error{Code: CodeIllegalTransition, Message: "success requires FINALIZING phase"}
	}
	if next == StatusCanceled && o.record.CancelRequestedAt.IsZero() {
		o.mu.Unlock()
		return &Error{Code: CodeIllegalTransition, Message: "canceled status requires an accepted cancel request"}
	}
	before := o.record
	beforeNotified := o.terminalNotified
	o.record.Status = next
	o.record.Revision++
	now := o.engine.config.Clock.Now()
	o.record.LastProgressAt = now
	if next == StatusRunning && o.record.StartedAt.IsZero() {
		o.record.StartedAt = now
	}
	o.record.Result = result
	o.record.Error = errorText
	if next == StatusInterrupted {
		o.record.PartialEffectsPossible = true
	}
	lease, finished, notify := o.finishLocked(next)
	if err := o.engine.persist(o.record, false); err != nil {
		o.record = before
		o.terminalNotified = beforeNotified
		if lease != nil {
			o.lease = lease
		}
		o.mu.Unlock()
		return err
	}
	o.mu.Unlock()
	o.afterTerminal(lease, finished, notify)
	return nil
}

func legalStatusTransition(current, next Status) bool {
	switch current {
	case StatusRequested:
		return next == StatusDispatched || next == StatusRejected || next == StatusCanceled || next == StatusInterrupted
	case StatusDispatched:
		return next == StatusRunning || next == StatusUnknown || next == StatusRejected || next == StatusCanceled || next == StatusInterrupted
	case StatusRunning:
		return next == StatusSuccess || next == StatusFailed || next == StatusCanceled || next == StatusInterrupted || next == StatusUnknown
	case StatusUnknown:
		return next == StatusRunning || next == StatusSuccess || next == StatusFailed || next == StatusCanceled || next == StatusInterrupted
	default:
		return false
	}
}

// AdvancePhase moves through the mode-specific phase graph. BEFORE_COMMIT
// Operations must use EnterCommit for the EXECUTING -> COMMITTING edge.
func (o *Operation) AdvancePhase(next Phase) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.record.Status.Terminal() {
		return &Error{Code: CodeIllegalTransition, Message: "terminal operation cannot advance phase"}
	}
	valid := o.record.Phase == PhasePreparing && next == PhaseExecuting
	if o.record.CancelMode == CancelBeforeCommit {
		valid = valid || o.record.Phase == PhaseCommitting && next == PhaseFinalizing
	} else {
		valid = valid || o.record.Phase == PhaseExecuting && next == PhaseFinalizing
	}
	if !valid {
		return &Error{Code: CodeIllegalTransition, Message: fmt.Sprintf("phase %s cannot transition to %s for %s", o.record.Phase, next, o.record.CancelMode)}
	}
	previousPhase := o.record.Phase
	previousProgress := o.record.LastProgressAt
	o.record.Phase = next
	o.record.Revision++
	o.record.LastProgressAt = o.engine.config.Clock.Now()
	if err := o.engine.persist(o.record, false); err != nil {
		o.record.Phase = previousPhase
		o.record.Revision--
		o.record.LastProgressAt = previousProgress
		return err
	}
	return nil
}

// EnterCommit and Cancel serialize on the same Operation mutex. Exactly one
// can win the pre-commit race.
func (o *Operation) EnterCommit() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.record.Status.Terminal() {
		return &Error{Code: CodeIllegalTransition, Message: "terminal operation cannot enter commit"}
	}
	if o.record.CancelMode != CancelBeforeCommit || o.record.Phase != PhaseExecuting {
		return &Error{Code: CodeIllegalTransition, Message: "commit requires a BEFORE_COMMIT operation in EXECUTING phase"}
	}
	if !o.record.CancelRequestedAt.IsZero() {
		return &Error{Code: CodeIllegalTransition, Message: "cancel was accepted before commit"}
	}
	now := o.engine.config.Clock.Now()
	before := o.record
	o.record.Phase = PhaseCommitting
	o.record.CommitStartedAt = now
	o.record.Revision++
	o.record.LastProgressAt = now
	if err := o.engine.persist(o.record, false); err != nil {
		o.record = before
		return err
	}
	return nil
}

func (o *Operation) Cancel(reason CancelReason) CancelOutcome {
	outcome, _ := o.CancelWithError(reason)
	return outcome
}

// CancelWithError is the durable cancellation API. Transport handlers should
// use it so ACCEPTED is returned only after the minimal journal is synced.
func (o *Operation) CancelWithError(reason CancelReason) (CancelOutcome, error) {
	o.mu.Lock()
	if o.record.Status.Terminal() {
		o.mu.Unlock()
		return CancelAlreadyTerminal, nil
	}
	if o.record.CancelMode == CancelNone {
		o.mu.Unlock()
		return CancelNotCancelable, nil
	}
	if o.record.CancelMode == CancelBeforeCommit &&
		(!o.record.CommitStartedAt.IsZero() || o.record.Phase == PhaseCommitting || o.record.Phase == PhaseFinalizing) {
		o.mu.Unlock()
		return CancelTooLate, nil
	}
	if !o.record.CancelRequestedAt.IsZero() {
		o.mu.Unlock()
		return CancelAccepted, nil
	}
	before := o.record
	now := o.engine.config.Clock.Now()
	o.record.CancelRequestedAt = now
	o.record.CancelReason = reason
	o.record.PartialEffectsPossible = o.record.CancelMode == CancelBestEffortPartial
	o.record.Revision++
	o.record.LastProgressAt = now
	if err := o.engine.persist(o.record, false); err != nil {
		o.record = before
		o.mu.Unlock()
		return CancelNotFound, err
	}
	cancel := o.executionCancel
	o.mu.Unlock()
	if cancel != nil {
		cancel(fmt.Errorf("operation canceled: %s", reason))
	}
	return CancelAccepted, nil
}

func (o *Operation) reject(cause error) error {
	o.mu.Lock()
	if o.record.Status.Terminal() {
		o.mu.Unlock()
		return nil
	}
	before := o.record
	beforeNotified := o.terminalNotified
	o.record.Status = StatusRejected
	o.record.Error = cause.Error()
	o.record.Revision++
	o.record.LastProgressAt = o.engine.config.Clock.Now()
	lease, finished, notify := o.finishLocked(StatusRejected)
	if err := o.engine.persist(o.record, false); err != nil {
		o.record = before
		o.terminalNotified = beforeNotified
		if lease != nil {
			o.lease = lease
		}
		o.mu.Unlock()
		return err
	}
	o.mu.Unlock()
	o.afterTerminal(lease, finished, notify)
	return nil
}

// Reject terminates an accepted operation before execution when a capability
// or storage-admission boundary denies it. The minimal record remains durable.
func (o *Operation) Reject(cause error) error {
	if cause == nil {
		cause = &Error{Code: CodeInvalidSpec, Message: "operation rejected"}
	}
	return o.reject(cause)
}

func (o *Operation) finishLocked(status Status) (*Lease, time.Time, bool) {
	if !status.Terminal() || o.terminalNotified {
		return nil, time.Time{}, false
	}
	now := o.engine.config.Clock.Now()
	o.record.FinishedAt = now
	if o.engine.config.TerminalAuditor != nil {
		o.record.ManagedAuditDelivery = ManagedAuditPending
	}
	o.terminalNotified = true
	lease := o.lease
	o.lease = nil
	return lease, now, true
}

func (o *Operation) afterTerminal(lease *Lease, finished time.Time, notify bool) {
	if !notify {
		return
	}
	lease.Release()
	o.engine.onTerminal(o, finished)
}

// WriteOutput implements an always-draining bounded tail. It always reports
// the entire input consumed while retaining only the newest configured bytes.
func (o *Operation) WriteOutput(payload []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	written := len(payload)
	if written == 0 {
		return 0, nil
	}
	if len(payload) >= o.outputLimit {
		if len(o.record.OutputTail) > 0 || len(payload) > o.outputLimit {
			o.record.OutputTruncated = true
		}
		o.record.OutputTail = append(o.record.OutputTail[:0], payload[len(payload)-o.outputLimit:]...)
		o.record.OutputTail = TrimPartialLeadingRune(o.record.OutputTail)
		return written, nil
	}
	o.record.OutputTail = append(o.record.OutputTail, payload...)
	if overflow := len(o.record.OutputTail) - o.outputLimit; overflow > 0 {
		copy(o.record.OutputTail, o.record.OutputTail[overflow:])
		o.record.OutputTail = TrimPartialLeadingRune(o.record.OutputTail[:o.outputLimit])
		o.record.OutputTruncated = true
	}
	return written, nil
}

// TrimPartialLeadingRune drops the leading bytes of a UTF-8 sequence that a
// byte-bounded tail cut in half.
//
// A tail keeps the newest bytes, so a truncation always cuts at the head and
// never in the middle of the retained text: everything after the first retained
// byte is contiguous. Without this trim an ordinary truncation can leave a
// dangling continuation byte at the front, and the Server rejects such a record
// as corrupt data. That answers 500 for every read of the operation, including
// a repeated idempotent cancel, for as long as the operation exists.
//
// At most utf8.UTFMax-1 bytes are removed. Input that carries no rune start at
// all is returned unchanged, so the Server's validity check still catches a
// genuinely non-textual record instead of being silently satisfied here.
func TrimPartialLeadingRune(tail []byte) []byte {
	for index := 0; index < len(tail) && index < utf8.UTFMax; index++ {
		if utf8.RuneStart(tail[index]) {
			return tail[index:]
		}
	}
	return tail
}

// AdvanceProgress records COMMITTING sub-step progress (for example, restore
// file 2/4 replaced) without inventing a new phase.
func (o *Operation) AdvanceProgress() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.record.Status.Terminal() {
		return &Error{Code: CodeIllegalTransition, Message: "terminal operation cannot advance progress"}
	}
	before := o.record
	o.record.Revision++
	o.record.LastProgressAt = o.engine.config.Clock.Now()
	if err := o.engine.persist(o.record, false); err != nil {
		o.record = before
		return err
	}
	return nil
}

// FlushOutputTail attempts optional output persistence. Admission denial is
// intentionally reported as ErrOutputPersistenceDropped and does not affect
// the durable minimal record.
func (o *Operation) FlushOutputTail() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.engine.persist(o.record, true)
}
