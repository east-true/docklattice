// Package diskbudget owns the Agent-wide storage pressure state, admission
// policy, emergency-reserve boundary, and fixed cleanup ordering.
package diskbudget

import (
	"context"
	"errors"
	"fmt"
	"sync"

	productconfig "github.com/east-true/docklattice/internal/config"
)

var (
	ErrInvalidObservation = errors.New("invalid disk observation")
	ErrStorageDegraded    = errors.New("DEGRADED_STORAGE")
	ErrDurableAdmission   = errors.New("DURABLE_ADMISSION_FAILED")
)

type DegradedReason string

const (
	ReasonNone                DegradedReason = ""
	ReasonFilesystemFreeLow   DegradedReason = "FILESYSTEM_FREE_LOW"
	ReasonAgentBudgetExceeded DegradedReason = "AGENT_STATE_BUDGET_EXCEEDED"
	ReasonBoth                DegradedReason = "BOTH"
)

type Observation struct {
	FilesystemTotalBytes int64
	FilesystemFreeBytes  int64
	AgentStateBytes      int64
}

type State struct {
	Degraded         bool
	Reason           DegradedReason
	EntryFreeFloor   int64
	ExitFreeFloor    int64
	StateBudget      int64
	ExitStateCeiling int64
}

type Manager struct {
	mu     sync.Mutex
	config Config
	state  State
}

type Config struct {
	StateBudgetBytes      int64
	EntryFreeBytes        int64
	EntryFreePercent      int
	ExitFreeBytes         int64
	ExitFreePercent       int
	ExitStatePercent      int
	EmergencyReserveBytes int64
}

func DefaultConfig() Config {
	defaults := productconfig.V1Defaults()
	return Config{
		StateBudgetBytes:      defaults.AgentStateMaxBytes,
		EntryFreeBytes:        defaults.FilesystemFreeMinBytes,
		EntryFreePercent:      defaults.FilesystemFreeMinPercent,
		ExitFreeBytes:         defaults.FilesystemFreeMinBytes * 6 / 5,
		ExitFreePercent:       6,
		ExitStatePercent:      90,
		EmergencyReserveBytes: defaults.EmergencyReserveBytes,
	}
}

func New(config Config) (*Manager, error) {
	if config.StateBudgetBytes <= 0 || config.EntryFreeBytes <= 0 || config.EntryFreePercent <= 0 ||
		config.ExitFreeBytes <= config.EntryFreeBytes || config.ExitFreePercent <= config.EntryFreePercent ||
		config.ExitStatePercent <= 0 || config.ExitStatePercent >= 100 || config.EmergencyReserveBytes <= 0 ||
		config.EmergencyReserveBytes >= config.StateBudgetBytes {
		return nil, fmt.Errorf("invalid disk budget configuration")
	}
	return &Manager{config: config}, nil
}

// Observe applies the asymmetric OR-entry/AND-exit hysteresis contract.
func (manager *Manager) Observe(observation Observation) (State, error) {
	if err := validateObservation(observation); err != nil {
		return State{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	previousReason := manager.state.Reason
	entryFloor := maximum(manager.config.EntryFreeBytes, percentBytes(observation.FilesystemTotalBytes, manager.config.EntryFreePercent))
	exitFloor := maximum(manager.config.ExitFreeBytes, percentBytes(observation.FilesystemTotalBytes, manager.config.ExitFreePercent))
	freeLow := observation.FilesystemFreeBytes < entryFloor
	budgetExceeded := observation.AgentStateBytes > manager.config.StateBudgetBytes
	if !manager.state.Degraded {
		manager.state.Degraded = freeLow || budgetExceeded
	} else {
		exitStateCeiling := manager.config.StateBudgetBytes * int64(manager.config.ExitStatePercent) / 100
		manager.state.Degraded = !(observation.FilesystemFreeBytes >= exitFloor && observation.AgentStateBytes <= exitStateCeiling)
	}
	manager.state.Reason = reason(freeLow, budgetExceeded)
	if manager.state.Degraded && manager.state.Reason == ReasonNone {
		// Hysteresis band: preserve the cause users must continue resolving.
		manager.state.Reason = previousReason
	}
	manager.state.EntryFreeFloor = entryFloor
	manager.state.ExitFreeFloor = exitFloor
	manager.state.StateBudget = manager.config.StateBudgetBytes
	manager.state.ExitStateCeiling = manager.config.StateBudgetBytes * int64(manager.config.ExitStatePercent) / 100
	return manager.state, nil
}

type Operation string

const (
	OperationQuery             Operation = "query"
	OperationFileRead          Operation = "file.read"
	OperationLogs              Operation = "logs"
	OperationMetrics           Operation = "metrics"
	OperationAuditSync         Operation = "audit.sync"
	OperationResultRead        Operation = "operation.read"
	OperationBackupList        Operation = "backup.list"
	OperationBackupDelete      Operation = "backup.delete"
	OperationComposeUp         Operation = "compose.up"
	OperationComposeDown       Operation = "compose.down"
	OperationComposeStart      Operation = "compose.start"
	OperationComposeStop       Operation = "compose.stop"
	OperationComposeRestart    Operation = "compose.restart"
	OperationContainerStart    Operation = "container.start"
	OperationContainerStop     Operation = "container.stop"
	OperationContainerRestart  Operation = "container.restart"
	OperationContainerRemove   Operation = "container.remove"
	OperationComposePull       Operation = "compose.pull"
	OperationFileWrite         Operation = "file.write"
	OperationBackupCreate      Operation = "backup.create"
	OperationBackupRestore     Operation = "backup.restore"
	OperationAutomaticSnapshot Operation = "snapshot.automatic"
)

type Admission struct {
	Allowed bool
	Reason  string
	Err     error
}

// Admit applies only the storage policy. Other capability, lock, and
// self-protection checks remain independent.
func Admit(state State, operation Operation, durableCapacity bool) Admission {
	if !validOperation(operation) {
		return Admission{Reason: "unknown operation", Err: ErrStorageDegraded}
	}
	if !state.Degraded {
		return Admission{Allowed: true}
	}
	if isRead(operation) || operation == OperationBackupDelete {
		return Admission{Allowed: true}
	}
	if isDegradedMutationAllowed(operation) {
		if !durableCapacity {
			return Admission{Reason: "minimum Operation and Managed Audit records cannot be reserved", Err: ErrDurableAdmission}
		}
		return Admission{Allowed: true}
	}
	return Admission{Reason: fmt.Sprintf("%s: %s", state.Reason, operation), Err: ErrStorageDegraded}
}

type WriteClass string

const (
	WriteAuditWAL          WriteClass = "AUDIT_WAL"
	WriteAuditCoverage     WriteClass = "AUDIT_COVERAGE"
	WriteContinuity        WriteClass = "AUDIT_CONTINUITY_UNCERTAIN"
	WriteOperationMinimum  WriteClass = "OPERATION_MINIMUM"
	WriteRestoreJournal    WriteClass = "RESTORE_JOURNAL"
	WriteAgentLifecycle    WriteClass = "AGENT_LIFECYCLE"
	WriteOperationOutput   WriteClass = "OPERATION_OUTPUT"
	WriteLogs              WriteClass = "LOGS"
	WriteMetrics           WriteClass = "METRICS"
	WriteBackup            WriteClass = "BACKUP"
	WriteAutomaticSnapshot WriteClass = "AUTOMATIC_SNAPSHOT"
	WriteStaging           WriteClass = "STAGING"
)

func (manager *Manager) CanWrite(freeBytes, bytes int64, class WriteClass) bool {
	if freeBytes < 0 || bytes < 0 || bytes > freeBytes {
		return false
	}
	if reserveEligible(class) {
		return true
	}
	return freeBytes-bytes >= manager.config.EmergencyReserveBytes
}

type Tier string

const (
	TierAbandonedTemp     Tier = "ABANDONED_TEMP"
	TierACKedWAL          Tier = "ACKED_WAL"
	TierExpiredOperations Tier = "EXPIRED_OPERATIONS"
	TierExcessSnapshots   Tier = "EXCESS_AUTOMATIC_SNAPSHOTS"
	TierOldSnapshots      Tier = "OLD_AUTOMATIC_SNAPSHOTS"
	TierUnackedWAL        Tier = "UNACKED_WAL_WITH_GAP"
)

type Reclaimer interface {
	Reclaim(context.Context, int64) (int64, error)
}

type ReclaimerFunc func(context.Context, int64) (int64, error)

func (function ReclaimerFunc) Reclaim(ctx context.Context, bytes int64) (int64, error) {
	return function(ctx, bytes)
}

type Reclaimers struct {
	AbandonedTemp     Reclaimer
	ACKedWAL          Reclaimer
	ExpiredOperations Reclaimer
	ExcessSnapshots   Reclaimer
	OldSnapshots      Reclaimer
	UnackedWAL        Reclaimer
}

type ReclaimStep struct {
	Tier       Tier
	FreedBytes int64
}

type ReclaimResult struct {
	RequestedBytes int64
	FreedBytes     int64
	Degraded       bool
	Steps          []ReclaimStep
}

// Reclaim executes the architecture's irreversible-loss ordering. Manual
// backup bytes have no callback and therefore cannot be silently selected.
// The OldSnapshots implementation must protect each project's newest one.
func Reclaim(ctx context.Context, bytesNeeded int64, reclaimers Reclaimers) (ReclaimResult, error) {
	if bytesNeeded <= 0 {
		return ReclaimResult{}, nil
	}
	result := ReclaimResult{RequestedBytes: bytesNeeded}
	ordered := []struct {
		tier      Tier
		reclaimer Reclaimer
	}{
		{TierAbandonedTemp, reclaimers.AbandonedTemp},
		{TierACKedWAL, reclaimers.ACKedWAL},
		{TierExpiredOperations, reclaimers.ExpiredOperations},
		{TierExcessSnapshots, reclaimers.ExcessSnapshots},
		{TierOldSnapshots, reclaimers.OldSnapshots},
		{TierUnackedWAL, reclaimers.UnackedWAL},
	}
	for _, item := range ordered {
		if result.FreedBytes >= bytesNeeded || item.reclaimer == nil {
			continue
		}
		remaining := bytesNeeded - result.FreedBytes
		freed, err := item.reclaimer.Reclaim(ctx, remaining)
		if freed < 0 {
			return result, fmt.Errorf("reclaim %s returned negative bytes", item.tier)
		}
		if freed > int64(^uint64(0)>>1)-result.FreedBytes {
			return result, fmt.Errorf("reclaim %s byte count overflow", item.tier)
		}
		result.FreedBytes += freed
		result.Steps = append(result.Steps, ReclaimStep{Tier: item.tier, FreedBytes: freed})
		if err != nil {
			return result, fmt.Errorf("reclaim %s: %w", item.tier, err)
		}
	}
	result.Degraded = result.FreedBytes < bytesNeeded
	return result, nil
}

func validateObservation(value Observation) error {
	if value.FilesystemTotalBytes <= 0 || value.FilesystemFreeBytes < 0 || value.FilesystemFreeBytes > value.FilesystemTotalBytes || value.AgentStateBytes < 0 {
		return ErrInvalidObservation
	}
	return nil
}

func reason(freeLow, budgetExceeded bool) DegradedReason {
	switch {
	case freeLow && budgetExceeded:
		return ReasonBoth
	case freeLow:
		return ReasonFilesystemFreeLow
	case budgetExceeded:
		return ReasonAgentBudgetExceeded
	default:
		return ReasonNone
	}
}

func isRead(operation Operation) bool {
	switch operation {
	case OperationQuery, OperationFileRead, OperationLogs, OperationMetrics, OperationAuditSync, OperationResultRead, OperationBackupList:
		return true
	default:
		return false
	}
}

func isDegradedMutationAllowed(operation Operation) bool {
	switch operation {
	case OperationComposeUp, OperationComposeDown, OperationComposeStart, OperationComposeStop, OperationComposeRestart,
		OperationContainerStart, OperationContainerStop, OperationContainerRestart, OperationContainerRemove:
		return true
	default:
		return false
	}
}

func validOperation(operation Operation) bool {
	return isRead(operation) || operation == OperationBackupDelete || isDegradedMutationAllowed(operation) ||
		operation == OperationComposePull || operation == OperationFileWrite || operation == OperationBackupCreate ||
		operation == OperationBackupRestore || operation == OperationAutomaticSnapshot
}

func reserveEligible(class WriteClass) bool {
	switch class {
	case WriteAuditWAL, WriteAuditCoverage, WriteContinuity, WriteOperationMinimum, WriteRestoreJournal, WriteAgentLifecycle:
		return true
	default:
		return false
	}
}

func maximum(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func percentBytes(total int64, percent int) int64 {
	return total * int64(percent) / 100
}
