package operation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/config"
)

const DefaultProjectLockWait = 2 * time.Second
const DefaultStalledAfter = 5 * time.Minute

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Config struct {
	Clock           Clock
	ProjectLockWait time.Duration
	ResultRetention time.Duration
	ResultMax       int
	OutputTailBytes int
	StalledAfter    time.Duration
	Journal         Journal
	TerminalAuditor TerminalAuditor
}

func DefaultConfig() Config {
	defaults := config.V1Defaults()
	return Config{
		Clock:           realClock{},
		ProjectLockWait: DefaultProjectLockWait,
		ResultRetention: defaults.OperationResultRetention,
		ResultMax:       defaults.OperationResultMax,
		OutputTailBytes: int(defaults.OperationOutputTailBytes),
		StalledAfter:    DefaultStalledAfter,
	}
}

func (c Config) validate() error {
	if c.Clock == nil {
		return &Error{Code: CodeInvalidSpec, Message: "clock is required"}
	}
	if c.ProjectLockWait < 0 || c.ResultRetention <= 0 || c.ResultMax <= 0 || c.OutputTailBytes <= 0 || c.StalledAfter <= 0 {
		return &Error{Code: CodeInvalidSpec, Message: "lock wait must be non-negative and result/output bounds must be positive"}
	}
	if c.TerminalAuditor != nil && c.Journal == nil {
		return &Error{Code: CodeInvalidSpec, Message: "terminal auditor requires a durable operation journal"}
	}
	return nil
}

type resultEntry struct {
	id         string
	finishedAt time.Time
}

// Engine owns operation ID idempotency, active/result indexing, and locks.
type Engine struct {
	mu           sync.Mutex
	config       Config
	locks        *LockManager
	items        map[string]*Operation
	results      []resultEntry
	shuttingDown bool
	runnerCount  int
	activity     chan struct{}
}

func New(config Config) (*Engine, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	engine := &Engine{
		config:   config,
		locks:    NewLockManager(config.ProjectLockWait),
		items:    make(map[string]*Operation),
		activity: make(chan struct{}),
	}
	if err := engine.recoverJournal(); err != nil {
		return nil, err
	}
	if err := engine.ReplayTerminalAudits(context.Background()); err != nil {
		return nil, fmt.Errorf("operation: replay terminal audits: %w", err)
	}
	return engine, nil
}

func NewDefault() *Engine {
	engine, err := New(DefaultConfig())
	if err != nil {
		panic(err)
	}
	return engine
}

// Create inserts one idempotent Operation and acquires its project lock when
// required. created=false means the exact existing spec was returned.
func (e *Engine) Create(ctx context.Context, spec Spec) (*Operation, bool, error) {
	operation, created, err := e.accept(spec, false)
	if err != nil || !created {
		return operation, created, err
	}
	if requiresProjectLock(spec.Type) {
		lease, err := e.locks.Acquire(ctx, spec.ProjectKey, spec.OperationID)
		if err != nil {
			if persistErr := operation.reject(err); persistErr != nil {
				return operation, true, errors.Join(err, persistErr)
			}
			return operation, true, err
		}
		operation.attachLease(lease)
	}
	return operation, true, nil
}

func (e *Engine) accept(spec Spec, reserveRunner bool) (*Operation, bool, error) {
	if spec.OperationID == "" || !spec.Type.Valid() || !validPayloadHash(spec.PayloadHash) {
		return nil, false, &Error{Code: CodeInvalidSpec, Message: "operation ID and a supported type are required"}
	}
	if requiresProjectLock(spec.Type) && spec.ProjectKey == "" {
		return nil, false, &Error{Code: CodeInvalidSpec, Message: "project key is required for this operation type"}
	}
	now := e.config.Clock.Now()
	e.mu.Lock()
	e.cleanupLocked(now)
	if e.shuttingDown {
		e.mu.Unlock()
		return nil, false, &Error{Code: CodeAgentShuttingDown, Message: "Agent operation engine is shutting down"}
	}
	if existing := e.items[spec.OperationID]; existing != nil {
		if existing.spec != spec {
			e.mu.Unlock()
			return nil, false, &Error{Code: CodeSpecMismatch, Message: fmt.Sprintf("operation ID %q was already used with a different spec", spec.OperationID)}
		}
		e.mu.Unlock()
		return existing, false, nil
	}
	operation := newOperation(e, spec, cancelModeForType(spec.Type), now, e.config.OutputTailBytes)
	if err := e.persist(operation.record, false); err != nil {
		e.mu.Unlock()
		return nil, false, err
	}
	e.items[spec.OperationID] = operation
	if reserveRunner {
		e.runnerCount++
		e.notifyActivityLocked()
	}
	e.mu.Unlock()
	return operation, true, nil
}

func (e *Engine) Cancel(operationID string, reason CancelReason) CancelOutcome {
	outcome, _ := e.CancelWithError(operationID, reason)
	return outcome
}

func (e *Engine) CancelWithError(operationID string, reason CancelReason) (CancelOutcome, error) {
	e.mu.Lock()
	operation := e.items[operationID]
	e.mu.Unlock()
	if operation == nil {
		return CancelNotFound, nil
	}
	return operation.CancelWithError(reason)
}

func (e *Engine) Get(operationID string) (Record, bool) {
	e.mu.Lock()
	e.cleanupLocked(e.config.Clock.Now())
	operation := e.items[operationID]
	e.mu.Unlock()
	if operation == nil {
		return Record{}, false
	}
	return operation.Snapshot(), true
}

// ListActiveOperations returns copy-safe nonterminal records ordered by ID.
func (e *Engine) ListActiveOperations() []Record {
	e.mu.Lock()
	e.cleanupLocked(e.config.Clock.Now())
	operations := make([]*Operation, 0, len(e.items))
	for _, operation := range e.items {
		operations = append(operations, operation)
	}
	e.mu.Unlock()
	records := make([]Record, 0, len(operations))
	for _, operation := range operations {
		if record := operation.Snapshot(); !record.Status.Terminal() {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].OperationID < records[j].OperationID })
	return records
}

type Runner func(context.Context, *Operation)

// StartOperation durably accepts an operation before starting lock acquisition
// and execution in the background. The caller's transport cancellation is
// detached; explicit Cancel remains the only cancellation contract.
func (e *Engine) StartOperation(ctx context.Context, spec Spec, runner Runner) (Record, bool, error) {
	if runner == nil {
		return Record{}, false, &Error{Code: CodeInvalidSpec, Message: "runner is required"}
	}
	operation, created, err := e.accept(spec, true)
	if err != nil || !created {
		if operation == nil {
			return Record{}, created, err
		}
		return operation.Snapshot(), created, err
	}
	accepted := operation.Snapshot()
	executionContext, cancelExecution := context.WithCancelCause(context.WithoutCancel(ctx))
	operation.attachExecutionCancel(cancelExecution)
	go func() {
		defer e.runnerFinished()
		defer cancelExecution(nil)
		if requiresProjectLock(spec.Type) {
			lease, acquireErr := e.locks.Acquire(executionContext, spec.ProjectKey, spec.OperationID)
			if acquireErr != nil {
				if !operation.Snapshot().CancelRequestedAt.IsZero() {
					_ = operation.TransitionStatus(StatusCanceled, "", acquireErr.Error())
					return
				}
				_ = operation.reject(acquireErr)
				return
			}
			operation.attachLease(lease)
		}
		if err := operation.TransitionStatus(StatusDispatched, "", ""); err != nil {
			_ = operation.reject(err)
			return
		}
		runner(executionContext, operation)
	}()
	return accepted, true, nil
}

// DisconnectKind exists to make the non-cancellation contract explicit.
type DisconnectKind string

const (
	DisconnectBrowser   DisconnectKind = "BROWSER"
	DisconnectTransport DisconnectKind = "TRANSPORT"
)

// HandleDisconnect intentionally leaves mutation Operations untouched.
func (e *Engine) HandleDisconnect(operationID string, kind DisconnectKind) bool {
	if kind != DisconnectBrowser && kind != DisconnectTransport {
		return false
	}
	_, exists := e.Get(operationID)
	return exists
}

func (e *Engine) onTerminal(operation *Operation, finishedAt time.Time) {
	e.mu.Lock()
	if e.items[operation.spec.OperationID] == operation {
		e.results = append(e.results, resultEntry{id: operation.spec.OperationID, finishedAt: finishedAt})
		e.cleanupLocked(e.config.Clock.Now())
	}
	e.notifyActivityLocked()
	e.mu.Unlock()
	_ = e.deliverTerminalAudit(context.Background(), operation)
}

func (e *Engine) runnerFinished() {
	e.mu.Lock()
	if e.runnerCount > 0 {
		e.runnerCount--
	}
	e.notifyActivityLocked()
	e.mu.Unlock()
}

func (e *Engine) notifyActivityLocked() {
	close(e.activity)
	e.activity = make(chan struct{})
}

func (e *Engine) cleanupLocked(now time.Time) {
	cutoff := now.Add(-e.config.ResultRetention)
	for len(e.results) > 0 && (len(e.results) > e.config.ResultMax || !e.results[0].finishedAt.After(cutoff)) {
		entry := e.results[0]
		if operation := e.items[entry.id]; operation != nil && operation.isTerminal() {
			if operation.auditDelivery() == ManagedAuditPending {
				return
			}
			if e.config.Journal != nil {
				if err := e.config.Journal.Delete(entry.id); err != nil {
					// Retain the in-memory result when durable deletion fails so a
					// restart cannot resurrect a result the engine claimed to evict.
					return
				}
			}
			delete(e.items, entry.id)
		}
		e.results = e.results[1:]
	}
}
