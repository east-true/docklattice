package agentstorage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/operation"
)

var ErrInvalidEvictionConfig = errors.New("agentstorage: invalid eviction configuration")

type WALDiskPressureReclaimer interface {
	ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error)
	ReclaimACKedForDiskPressure(int64) (auditwal.ReclaimResult, error)
	ReclaimUnackedForDiskPressure(int64) (auditwal.ReclaimResult, error)
}

type OperationDiskPressureReclaimer interface {
	ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error)
	ReclaimExpiredForDiskPressure(context.Context, int64) (int64, error)
}

type BackupDiskPressureReclaimer interface {
	ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error)
	ReclaimExcessAutomaticForDiskPressure(context.Context, int64, int) (int64, error)
	ReclaimOldAutomaticForDiskPressure(context.Context, int64) (int64, error)
}

type FileStagingDiskPressureReclaimer interface {
	ReclaimAbandonedStagingForDiskPressure(context.Context, int64) (int64, error)
}

var (
	_ WALDiskPressureReclaimer       = (*auditwal.WAL)(nil)
	_ OperationDiskPressureReclaimer = (*operation.Engine)(nil)
	_ BackupDiskPressureReclaimer    = (*backup.Manager)(nil)
)

type EvictionConfig struct {
	WAL                        WALDiskPressureReclaimer
	Operations                 OperationDiskPressureReclaimer
	Backups                    BackupDiskPressureReclaimer
	FileStaging                FileStagingDiskPressureReclaimer
	AutomaticSnapshotsToRetain int
}

// EvictionExecutor is the single serialized owner of the Agent-wide §14.1
// cleanup sequence. Subsystems retain their own locks so ordinary concurrent
// writes cannot be mistaken for abandoned or retention-eligible data.
type EvictionExecutor struct {
	mu         sync.Mutex
	wal        WALDiskPressureReclaimer
	operations OperationDiskPressureReclaimer
	backups    BackupDiskPressureReclaimer
	staging    FileStagingDiskPressureReclaimer
	keep       int
}

type PressureReclaimResult struct {
	BeforeObservation diskbudget.Observation
	BeforeState       diskbudget.State
	Reclaim           diskbudget.ReclaimResult
	AfterObservation  diskbudget.Observation
	AfterState        diskbudget.State
}

func NewEvictionExecutor(config EvictionConfig) (*EvictionExecutor, error) {
	if config.WAL == nil || config.Operations == nil || config.Backups == nil || config.FileStaging == nil {
		return nil, ErrInvalidEvictionConfig
	}
	keep := config.AutomaticSnapshotsToRetain
	if keep == 0 {
		keep = backup.AutomaticSnapshotRetention
	}
	if keep < 1 {
		return nil, ErrInvalidEvictionConfig
	}
	return &EvictionExecutor{wal: config.WAL, operations: config.Operations, backups: config.Backups, staging: config.FileStaging, keep: keep}, nil
}

// Reclaim executes every eligible tier until bytesNeeded has been reclaimed.
// Degraded=true means even the final unACKed-WAL-with-durable-gap tier was
// insufficient; no additional automatic deletion (especially no manual
// backup deletion) is attempted.
func (executor *EvictionExecutor) Reclaim(ctx context.Context, bytesNeeded int64) (diskbudget.ReclaimResult, error) {
	if bytesNeeded <= 0 {
		return diskbudget.ReclaimResult{}, nil
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return diskbudget.ReclaimResult{RequestedBytes: bytesNeeded, Degraded: true}, err
	}
	reclaimers := diskbudget.Reclaimers{
		AbandonedTemp: diskbudget.ReclaimerFunc(executor.reclaimAbandoned),
		ACKedWAL: diskbudget.ReclaimerFunc(func(ctx context.Context, bytes int64) (int64, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			result, err := executor.wal.ReclaimACKedForDiskPressure(bytes)
			return result.FreedBytes, err
		}),
		ExpiredOperations: diskbudget.ReclaimerFunc(executor.operations.ReclaimExpiredForDiskPressure),
		ExcessSnapshots: diskbudget.ReclaimerFunc(func(ctx context.Context, bytes int64) (int64, error) {
			return executor.backups.ReclaimExcessAutomaticForDiskPressure(ctx, bytes, executor.keep)
		}),
		OldSnapshots: diskbudget.ReclaimerFunc(executor.backups.ReclaimOldAutomaticForDiskPressure),
		UnackedWAL: diskbudget.ReclaimerFunc(func(ctx context.Context, bytes int64) (int64, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			result, err := executor.wal.ReclaimUnackedForDiskPressure(bytes)
			return result.FreedBytes, err
		}),
	}
	return diskbudget.Reclaim(ctx, bytesNeeded, reclaimers)
}

// ReclaimForPressure computes the bytes needed to reach both configured exit
// watermarks, runs the exact global sequence, then re-observes storage. It does
// not hold Controller.mu while subsystem locks are acquired, avoiding an
// admission-vs-backup lock inversion.
func (controller *Controller) ReclaimForPressure(ctx context.Context, executor *EvictionExecutor) (PressureReclaimResult, error) {
	if executor == nil {
		return PressureReclaimResult{}, ErrInvalidEvictionConfig
	}
	controller.mu.Lock()
	beforeState, err := controller.refreshLocked(ctx)
	beforeObservation := controller.observation
	controller.mu.Unlock()
	result := PressureReclaimResult{BeforeObservation: beforeObservation, BeforeState: beforeState}
	if err != nil {
		return result, err
	}
	if !beforeState.Degraded {
		result.AfterObservation, result.AfterState = beforeObservation, beforeState
		return result, nil
	}
	freeDeficit := beforeState.ExitFreeFloor - beforeObservation.FilesystemFreeBytes
	stateDeficit := beforeObservation.AgentStateBytes - beforeState.ExitStateCeiling
	bytesNeeded := maximumInt64(freeDeficit, stateDeficit)
	if bytesNeeded < 0 {
		bytesNeeded = 0
	}
	result.Reclaim, err = executor.Reclaim(ctx, bytesNeeded)

	controller.mu.Lock()
	afterState, refreshErr := controller.refreshLocked(ctx)
	result.AfterObservation = controller.observation
	result.AfterState = afterState
	controller.mu.Unlock()
	return result, errors.Join(err, refreshErr)
}

func (executor *EvictionExecutor) reclaimAbandoned(ctx context.Context, bytesNeeded int64) (int64, error) {
	var freed int64
	// Project staging is cleaned first, but never credited to this state-root
	// target. See ProjectStagingReclaimer: it may live on another filesystem and
	// is always outside AgentStateBytes.
	if _, err := executor.staging.ReclaimAbandonedStagingForDiskPressure(ctx, bytesNeeded); err != nil {
		return 0, err
	}
	for _, reclaim := range []func(context.Context, int64) (int64, error){
		executor.operations.ReclaimAbandonedTempForDiskPressure,
		executor.backups.ReclaimAbandonedTempForDiskPressure,
		executor.wal.ReclaimAbandonedTempForDiskPressure,
	} {
		if freed >= bytesNeeded {
			break
		}
		bytes, err := reclaim(ctx, bytesNeeded-freed)
		if bytes < 0 || bytes > math.MaxInt64-freed {
			return freed, fmt.Errorf("agentstorage: invalid abandoned-temp byte count %d", bytes)
		}
		freed += bytes
		if err != nil {
			return freed, err
		}
	}
	return freed, nil
}

func maximumInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
