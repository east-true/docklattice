package auditevents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/east-true/docklattice/internal/auditgen"
	"github.com/east-true/docklattice/internal/auditwal"
)

var ErrAppenderUnavailable = errors.New("AUDIT_APPENDER_UNAVAILABLE")

type WAL interface {
	Append(context.Context, []byte) (auditwal.Record, error)
}

type idempotentWAL interface {
	AppendOnce(context.Context, string, []byte) (auditwal.Record, error)
	ForgetOnce(string) error
}

// Appender is deliberately synchronous: WAL backpressure reaches the Docker
// event reader instead of creating an unbounded in-memory queue.
type Appender struct{ wal WAL }

func NewAppender(wal WAL) (*Appender, error) {
	if wal == nil {
		return nil, ErrAppenderUnavailable
	}
	return &Appender{wal: wal}, nil
}

// Append accepts observed and EVENT_STORM events. Managed operation callers
// must use AppendManaged or AppendEnvelope so operation_id cannot be omitted.
func (appender *Appender) Append(ctx context.Context, event auditgen.Event) (auditwal.Record, error) {
	return appender.AppendEnvelope(ctx, Envelope{Event: event})
}

func (appender *Appender) AppendEnvelope(ctx context.Context, envelope Envelope) (auditwal.Record, error) {
	if appender == nil || appender.wal == nil {
		return auditwal.Record{}, ErrAppenderUnavailable
	}
	payload, err := EncodeEnvelope(envelope)
	if err != nil {
		return auditwal.Record{}, err
	}
	record, err := appender.wal.Append(ctx, payload)
	if err != nil {
		return record, fmt.Errorf("append audit event: %w", err)
	}
	return record, nil
}

// AppendManaged is the operation-lifecycle adapter. Call it exactly once when
// an Operation reaches its durable terminal state; managed records bypass the
// observed-event generator and therefore are never rate limited.
func (appender *Appender) AppendManaged(ctx context.Context, signal auditgen.Signal, actor, projectUID, operationID string) (auditwal.Record, error) {
	event, err := auditgen.Managed(signal, actor)
	if err != nil {
		return auditwal.Record{}, err
	}
	return appender.AppendEnvelope(ctx, Envelope{Event: event, ProjectUID: projectUID, OperationID: operationID})
}

// AppendManagedOnce is the durable-outbox delivery boundary. The operation ID
// is namespaced into an idempotency key retained by the WAL until the caller
// durably records delivery and calls ConfirmManaged.
func (appender *Appender) AppendManagedOnce(ctx context.Context, signal auditgen.Signal, actor, projectUID, operationID string) (auditwal.Record, error) {
	if appender == nil || appender.wal == nil {
		return auditwal.Record{}, ErrAppenderUnavailable
	}
	idempotent, ok := appender.wal.(idempotentWAL)
	if !ok {
		return auditwal.Record{}, fmt.Errorf("%w: WAL does not support idempotent managed delivery", ErrAppenderUnavailable)
	}
	payload, err := EncodeManaged(signal, actor, projectUID, operationID)
	if err != nil {
		return auditwal.Record{}, err
	}
	record, err := idempotent.AppendOnce(ctx, managedOnceKey(operationID), payload)
	if err != nil {
		return record, fmt.Errorf("append managed audit event: %w", err)
	}
	return record, nil
}

// ConfirmManaged releases the WAL receipt after the source outbox has synced
// its DELIVERED marker. It is safe to repeat.
func (appender *Appender) ConfirmManaged(operationID string) error {
	if appender == nil || appender.wal == nil {
		return ErrAppenderUnavailable
	}
	idempotent, ok := appender.wal.(idempotentWAL)
	if !ok {
		return fmt.Errorf("%w: WAL does not support idempotent managed delivery", ErrAppenderUnavailable)
	}
	return idempotent.ForgetOnce(managedOnceKey(operationID))
}

func managedOnceKey(operationID string) string { return "managed-operation:" + operationID }

func (appender *Appender) AppendContinuityUncertain(ctx context.Context, previousIncarnation uint64, knownDurableThrough *uint64, at time.Time) (auditwal.Record, error) {
	if appender == nil || appender.wal == nil {
		return auditwal.Record{}, ErrAppenderUnavailable
	}
	payload, err := EncodeContinuityUncertain(previousIncarnation, knownDurableThrough, at)
	if err != nil {
		return auditwal.Record{}, err
	}
	record, err := appender.wal.Append(ctx, payload)
	if err != nil {
		return record, fmt.Errorf("append audit continuity boundary: %w", err)
	}
	return record, nil
}
