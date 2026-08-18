package operation

import (
	"context"
	"errors"
	"sort"
	"time"
)

const shutdownAuditRetryInterval = 50 * time.Millisecond

// Shutdown closes admission, requests AGENT_SHUTDOWN cancellation through the
// ordinary cancellation state machine, and waits for runner cleanup, durable
// terminal records, and managed-audit delivery. It never fabricates a
// terminal status for a runner: TOO_LATE COMMITTING operations must finish
// naturally, and a stuck runner makes the supplied context the hard bound.
// A non-nil return caused by the context means callers must keep the Journal,
// Audit WAL, Docker adapter, and other runner dependencies open; a runner may
// still be completing against them.
//
// Browser and transport disconnect handling remains independent and does not
// call this process-lifecycle boundary.
func (e *Engine) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return &Error{Code: CodeInvalidSpec, Message: "shutdown context is required"}
	}

	e.mu.Lock()
	e.shuttingDown = true
	operations := make([]*Operation, 0, len(e.items))
	for _, current := range e.items {
		if !current.isTerminal() {
			operations = append(operations, current)
		}
	}
	e.notifyActivityLocked()
	e.mu.Unlock()
	sort.Slice(operations, func(i, j int) bool { return operations[i].spec.OperationID < operations[j].spec.OperationID })

	var cancelErrors error
	for _, current := range operations {
		_, err := e.CancelWithError(current.spec.OperationID, CancelReasonAgentShutdown)
		cancelErrors = errors.Join(cancelErrors, err)
	}

	if err := e.waitForTerminalRunners(ctx); err != nil {
		return errors.Join(cancelErrors, err)
	}
	for {
		replayErr := e.ReplayTerminalAudits(ctx)
		if replayErr == nil {
			return cancelErrors
		}
		timer := time.NewTimer(shutdownAuditRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(cancelErrors, replayErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (e *Engine) waitForTerminalRunners(ctx context.Context) error {
	for {
		e.mu.Lock()
		active := false
		for _, current := range e.items {
			if !current.isTerminal() {
				active = true
				break
			}
		}
		runners := e.runnerCount
		activity := e.activity
		e.mu.Unlock()
		if !active && runners == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activity:
		}
	}
}
