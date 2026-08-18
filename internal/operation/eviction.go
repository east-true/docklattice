package operation

import (
	"context"
	"errors"
	"fmt"
)

var ErrDiskPressureReclaimUnsupported = errors.New("operation journal does not support disk-pressure reclaim")

// ReclaimAbandonedTempForDiskPressure removes interrupted journal temporary
// files without racing a concurrent Save.
func (e *Engine) ReclaimAbandonedTempForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 || e.config.Journal == nil {
		return 0, nil
	}
	journal, ok := e.config.Journal.(DiskPressureJournal)
	if !ok {
		return 0, ErrDiskPressureReclaimUnsupported
	}
	return journal.ReclaimAbandonedTempForDiskPressure(ctx, bytesNeeded)
}

// ReclaimExpiredForDiskPressure evicts only terminal results whose configured
// retention has elapsed. Active operations and terminal records whose managed
// Audit delivery is still pending are never candidates.
func (e *Engine) ReclaimExpiredForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if e.config.Journal == nil {
		return 0, nil
	}
	journal, ok := e.config.Journal.(DiskPressureJournal)
	if !ok {
		return 0, ErrDiskPressureReclaimUnsupported
	}
	cutoff := e.config.Clock.Now().Add(-e.config.ResultRetention)
	var freed int64
	remaining := e.results[:0]
	for index, entry := range e.results {
		if freed >= bytesNeeded || entry.finishedAt.After(cutoff) {
			remaining = append(remaining, e.results[index:]...)
			break
		}
		if err := ctx.Err(); err != nil {
			remaining = append(remaining, e.results[index:]...)
			e.results = remaining
			return freed, err
		}
		operation := e.items[entry.id]
		if operation == nil {
			continue
		}
		if !operation.isTerminal() || operation.auditDelivery() == ManagedAuditPending {
			remaining = append(remaining, entry)
			continue
		}
		itemBytes, recordDeleted, err := journal.ReclaimOperationForDiskPressure(ctx, entry.id)
		freed += itemBytes
		if recordDeleted {
			delete(e.items, entry.id)
		} else {
			remaining = append(remaining, entry)
		}
		if err != nil {
			remaining = append(remaining, e.results[index+1:]...)
			e.results = remaining
			return freed, fmt.Errorf("operation: reclaim %q: %w", entry.id, err)
		}
		if !recordDeleted {
			remaining = append(remaining, e.results[index+1:]...)
			e.results = remaining
			return freed, fmt.Errorf("operation: reclaim %q: journal record was not deleted", entry.id)
		}
	}
	e.results = remaining
	return freed, nil
}
