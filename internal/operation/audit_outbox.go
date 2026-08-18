package operation

import (
	"context"
	"errors"
	"sort"
)

// TerminalAuditor is an idempotent destination for the Operation journal's
// durable terminal-audit outbox. DeliverTerminal must retain its destination
// receipt until ConfirmTerminal is called after DELIVERED is journaled.
type TerminalAuditor interface {
	DeliverTerminal(context.Context, Record) error
	ConfirmTerminal(context.Context, string) error
}

// ReplayTerminalAudits delivers every pending terminal record in stable ID
// order. Engine startup invokes it before returning, and callers may invoke it
// again after a live WAL/storage error. Delivery and confirmation are both
// idempotent.
func (e *Engine) ReplayTerminalAudits(ctx context.Context) error {
	if e.config.TerminalAuditor == nil {
		return nil
	}
	e.mu.Lock()
	operations := make([]*Operation, 0, len(e.items))
	for _, current := range e.items {
		if current.isTerminal() && current.auditDelivery() != ManagedAuditNone {
			operations = append(operations, current)
		}
	}
	e.mu.Unlock()
	sort.Slice(operations, func(i, j int) bool { return operations[i].spec.OperationID < operations[j].spec.OperationID })
	var result error
	for _, current := range operations {
		if err := e.deliverTerminalAudit(ctx, current); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// PendingTerminalAudits exposes copy-safe pending records for health and
// shutdown gating without exposing the journal implementation.
func (e *Engine) PendingTerminalAudits() []Record {
	e.mu.Lock()
	operations := make([]*Operation, 0, len(e.items))
	for _, current := range e.items {
		if current.auditDelivery() == ManagedAuditPending {
			operations = append(operations, current)
		}
	}
	e.mu.Unlock()
	records := make([]Record, 0, len(operations))
	for _, current := range operations {
		records = append(records, current.Snapshot())
	}
	sort.Slice(records, func(i, j int) bool { return records[i].OperationID < records[j].OperationID })
	return records
}

func (e *Engine) deliverTerminalAudit(ctx context.Context, current *Operation) error {
	if e.config.TerminalAuditor == nil {
		return nil
	}
	current.auditMu.Lock()
	defer current.auditMu.Unlock()

	current.mu.Lock()
	record := current.record
	current.mu.Unlock()
	if record.ManagedAuditDelivery == ManagedAuditNone {
		return nil
	}
	if record.ManagedAuditDelivery == ManagedAuditDelivered {
		return e.config.TerminalAuditor.ConfirmTerminal(ctx, record.OperationID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.config.TerminalAuditor.DeliverTerminal(ctx, record); err != nil {
		return err
	}

	current.mu.Lock()
	before := current.record
	if current.record.ManagedAuditDelivery == ManagedAuditPending {
		current.record.ManagedAuditDelivery = ManagedAuditDelivered
		if err := e.persist(current.record, false); err != nil {
			current.record = before
			current.mu.Unlock()
			return err
		}
	}
	current.mu.Unlock()
	return e.config.TerminalAuditor.ConfirmTerminal(ctx, record.OperationID)
}
