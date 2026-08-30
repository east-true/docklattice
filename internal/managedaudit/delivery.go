// Package managedaudit maps durable terminal Operation records to exactly one
// managed Audit WAL record.
package managedaudit

import (
	"context"
	"strconv"

	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditgen"
	"github.com/east-true/docklattice/internal/operation"
)

// Delivery implements operation.TerminalAuditor. Managed records bypass the
// observed generator, so coalescing and rate limits can never apply.
type Delivery struct{ appender *auditevents.Appender }

func New(appender *auditevents.Appender) (*Delivery, error) {
	if appender == nil {
		return nil, auditevents.ErrAppenderUnavailable
	}
	return &Delivery{appender: appender}, nil
}

var _ operation.TerminalAuditor = (*Delivery)(nil)

func (delivery *Delivery) DeliverTerminal(ctx context.Context, record operation.Record) error {
	signal, projectUID := signalFor(record)
	_, err := delivery.appender.AppendManagedOnce(ctx, signal, "", projectUID, record.OperationID)
	return err
}

func (delivery *Delivery) ConfirmTerminal(_ context.Context, operationID string) error {
	return delivery.appender.ConfirmManaged(operationID)
}

func signalFor(record operation.Record) (auditgen.Signal, string) {
	resourceType, resourceID, projectUID := "operation", record.OperationID, ""
	switch record.Type {
	case operation.TypeContainerStart, operation.TypeContainerStop, operation.TypeContainerRestart, operation.TypeContainerRemove:
		resourceType, resourceID = "container", record.Target
	case operation.TypeDiscoveryRescan:
		resourceType, resourceID = "agent", "discovery"
	default:
		resourceType, resourceID, projectUID = "project", record.ProjectKey, record.ProjectKey
	}
	if resourceID == "" {
		// A malformed executor target must not make the terminal audit
		// permanently undeliverable; operation_id remains an exact identity.
		resourceType, resourceID = "operation", record.OperationID
	}
	attributes := map[string]string{
		"status":                   string(record.Status),
		"phase":                    string(record.Phase),
		"revision":                 strconv.FormatUint(record.Revision, 10),
		"partial_effects_possible": strconv.FormatBool(record.PartialEffectsPossible),
	}
	if record.CancelReason != "" {
		attributes["cancel_reason"] = string(record.CancelReason)
	}
	return auditgen.Signal{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Action:       string(record.Type),
		OccurredAt:   record.FinishedAt,
		Attributes:   attributes,
	}, projectUID
}
