package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/east-true/docklattice/internal/agentstorage"
	"github.com/east-true/docklattice/internal/diskbudget"
)

const storageReclaimMaxPasses = 8

type runtimeStorage struct {
	mu         sync.Mutex
	controller *agentstorage.Controller
	executor   *agentstorage.EvictionExecutor
	timeout    time.Duration
	status     agentstorage.PressureReclaimResult
	err        error
}

func newRuntimeStorage(controller *agentstorage.Controller, executor *agentstorage.EvictionExecutor, timeout time.Duration) (*runtimeStorage, error) {
	if controller == nil || executor == nil || timeout <= 0 {
		return nil, errors.New("agentruntime: storage controller and eviction executor are required")
	}
	return &runtimeStorage{controller: controller, executor: executor, timeout: timeout}, nil
}

// reclaimUntilStable is bounded by both caller context and a fixed pass cap.
// It stops earlier on exit, exhausted candidates, or an observation with no
// measurable state-root progress.
func (storage *runtimeStorage) reclaimUntilStable(ctx context.Context) (agentstorage.PressureReclaimResult, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	reclaimCtx, cancel := context.WithTimeout(ctx, storage.timeout)
	defer cancel()
	var latest agentstorage.PressureReclaimResult
	for pass := 0; pass < storageReclaimMaxPasses; pass++ {
		result, err := storage.controller.ReclaimForPressure(reclaimCtx, storage.executor)
		latest = result
		storage.status, storage.err = result, err
		if err != nil {
			return result, err
		}
		if !result.AfterState.Degraded || result.Reclaim.Degraded || !storageProgress(result) {
			return result, nil
		}
	}
	return latest, nil
}

// AdmitOperation gives a denied degraded-storage decision one serialized,
// exact-order reclaim attempt, then asks the authoritative policy again.
func (storage *runtimeStorage) AdmitOperation(ctx context.Context, kind diskbudget.Operation) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	initial := storage.controller.AdmitOperation(ctx, kind)
	if initial == nil || !errors.Is(initial, agentstorage.ErrAdmission) {
		return initial
	}
	reclaimCtx, cancel := context.WithTimeout(ctx, storage.timeout)
	defer cancel()
	result, reclaimErr := storage.controller.ReclaimForPressure(reclaimCtx, storage.executor)
	storage.status, storage.err = result, reclaimErr
	if reclaimErr != nil {
		return errors.Join(initial, fmt.Errorf("agentruntime: storage reclaim before admission: %w", reclaimErr))
	}
	return storage.controller.AdmitOperation(ctx, kind)
}

func (storage *runtimeStorage) AdmitProjectStaging(ctx context.Context, total, free, bytes int64) error {
	return storage.controller.AdmitProjectStaging(ctx, total, free, bytes)
}

func (storage *runtimeStorage) snapshot() (agentstorage.PressureReclaimResult, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return storage.status, storage.err
}

func storageProgress(result agentstorage.PressureReclaimResult) bool {
	return result.AfterObservation.AgentStateBytes < result.BeforeObservation.AgentStateBytes ||
		result.AfterObservation.FilesystemFreeBytes > result.BeforeObservation.FilesystemFreeBytes
}
