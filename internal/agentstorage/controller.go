// Package agentstorage connects the pure diskbudget policy to the Agent's
// real state filesystem and to operation/backup persistence admission.
package agentstorage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/operation"
)

// MinimumOperationDurableBytes is an admission accounting bound, not a new
// retention policy. It reserves one maximum operation journal object plus one
// maximum managed Audit payload before a degraded-storage mutation starts.
const MinimumOperationDurableBytes int64 = (1 << 20) + auditevents.MaxPayloadBytes

var ErrAdmission = errors.New("AGENT_STORAGE_ADMISSION_DENIED")
var ErrProjectFilesystemAdmission = errors.New("PROJECT_FILESYSTEM_ADMISSION_DENIED")

type Observer func(context.Context, string) (diskbudget.Observation, error)

type Config struct {
	StateRoot string
	Budget    diskbudget.Config
	Observe   Observer
}

type Controller struct {
	root    string
	manager *diskbudget.Manager
	observe Observer
	budget  diskbudget.Config

	mu          sync.Mutex
	observation diskbudget.Observation
	state       diskbudget.State
}

var (
	_ operation.PersistenceAdmitter = (*Controller)(nil)
	_ backup.BudgetAdmitter         = (*Controller)(nil)
)

func New(config Config) (*Controller, error) {
	root, err := filepath.Abs(config.StateRoot)
	if err != nil || root != filepath.Clean(config.StateRoot) {
		return nil, fmt.Errorf("agentstorage: state root must be absolute and clean")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("agentstorage: inspect state root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("agentstorage: state root must be a non-symlink directory")
	}
	budget := config.Budget
	if budget == (diskbudget.Config{}) {
		budget = diskbudget.DefaultConfig()
	}
	manager, err := diskbudget.New(budget)
	if err != nil {
		return nil, err
	}
	observer := config.Observe
	if observer == nil {
		observer = observeFilesystem
	}
	return &Controller{root: root, manager: manager, observe: observer, budget: budget}, nil
}

func (controller *Controller) Refresh(ctx context.Context) (diskbudget.State, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.refreshLocked(ctx)
}

func (controller *Controller) Snapshot() (diskbudget.Observation, diskbudget.State) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.observation, controller.state
}

func (controller *Controller) AdmitOperation(ctx context.Context, kind diskbudget.Operation) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	state, err := controller.refreshLocked(ctx)
	if err != nil {
		return err
	}
	durable := controller.manager.CanWrite(controller.observation.FilesystemFreeBytes, MinimumOperationDurableBytes, diskbudget.WriteOperationMinimum)
	admission := diskbudget.Admit(state, kind, durable)
	if !admission.Allowed {
		return errors.Join(ErrAdmission, admission.Err, errors.New(admission.Reason))
	}
	return nil
}

func (controller *Controller) AdmitOperationPersistence(ctx context.Context, request operation.PersistenceAdmission) error {
	class := diskbudget.WriteOperationMinimum
	if request.Class == operation.PersistenceOutput {
		class = diskbudget.WriteOperationOutput
	} else if request.Class != operation.PersistenceMinimal {
		return errors.Join(ErrAdmission, errors.New("unknown operation persistence class"))
	}
	return controller.admitBytes(ctx, request.EstimatedBytes, class)
}

func (controller *Controller) AdmitBackup(ctx context.Context, request backup.Admission) error {
	operationKind := diskbudget.OperationBackupCreate
	class := diskbudget.WriteBackup
	if request.Trigger != backup.TriggerManual {
		operationKind = diskbudget.OperationAutomaticSnapshot
		class = diskbudget.WriteAutomaticSnapshot
	}
	return controller.admitMutationBytes(ctx, operationKind, request.EstimatedBytes, class)
}

func (controller *Controller) AdmitRestore(ctx context.Context, request backup.RestoreAdmission) error {
	controller.mu.Lock()
	state, err := controller.refreshLocked(ctx)
	if err == nil {
		policy := diskbudget.Admit(state, diskbudget.OperationBackupRestore, false)
		if !policy.Allowed {
			err = errors.Join(ErrAdmission, policy.Err, errors.New(policy.Reason))
		}
	}
	controller.mu.Unlock()
	if err != nil {
		return err
	}
	return controller.AdmitProjectStaging(ctx, request.FilesystemTotalBytes, request.FilesystemFreeBytes, request.EstimatedBytes)
}

// AdmitProjectStaging applies the existing v1 filesystem-free entry floor to
// bytes staged beside a managed project. It introduces no project-volume
// quota/default and does not conflate those bytes with AgentStateBytes.
func (controller *Controller) AdmitProjectStaging(ctx context.Context, total, free, bytes int64) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrProjectFilesystemAdmission, err)
	}
	if total <= 0 || free < 0 || free > total || bytes < 0 {
		return errors.Join(ErrProjectFilesystemAdmission, diskbudget.ErrInvalidObservation)
	}
	entryFloor := maximumInt64(controller.budget.EntryFreeBytes, percentOf(total, controller.budget.EntryFreePercent))
	if bytes > free || free-bytes < entryFloor {
		return errors.Join(ErrProjectFilesystemAdmission, diskbudget.ErrStorageDegraded,
			fmt.Errorf("project filesystem free bytes would fall below %d", entryFloor))
	}
	return nil
}

func (controller *Controller) admitMutationBytes(ctx context.Context, kind diskbudget.Operation, bytes int64, class diskbudget.WriteClass) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	state, err := controller.refreshLocked(ctx)
	if err != nil {
		return err
	}
	policy := diskbudget.Admit(state, kind, false)
	if !policy.Allowed {
		return errors.Join(ErrAdmission, policy.Err, errors.New(policy.Reason))
	}
	if !controller.manager.CanWrite(controller.observation.FilesystemFreeBytes, bytes, class) {
		return errors.Join(ErrAdmission, diskbudget.ErrDurableAdmission)
	}
	return nil
}

func (controller *Controller) admitBytes(ctx context.Context, bytes int64, class diskbudget.WriteClass) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if _, err := controller.refreshLocked(ctx); err != nil {
		return err
	}
	if !controller.manager.CanWrite(controller.observation.FilesystemFreeBytes, bytes, class) {
		if class == diskbudget.WriteOperationOutput {
			return errors.Join(operation.ErrOutputPersistenceDropped, ErrAdmission)
		}
		return errors.Join(ErrAdmission, diskbudget.ErrDurableAdmission)
	}
	return nil
}

func (controller *Controller) refreshLocked(ctx context.Context) (diskbudget.State, error) {
	if err := ctx.Err(); err != nil {
		return diskbudget.State{}, err
	}
	observation, err := controller.observe(ctx, controller.root)
	if err != nil {
		return diskbudget.State{}, fmt.Errorf("agentstorage: observe: %w", err)
	}
	state, err := controller.manager.Observe(observation)
	if err != nil {
		return diskbudget.State{}, err
	}
	controller.observation, controller.state = observation, state
	return state, nil
}

func percentOf(bytes int64, percent int) int64 {
	return bytes * int64(percent) / 100
}
