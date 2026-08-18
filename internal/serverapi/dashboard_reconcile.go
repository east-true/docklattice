package serverapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/webui"
)

const dashboardAgentReconcileTimeout = 5 * time.Second

type dashboardReconcileResult struct {
	discoveryReasons        map[string]string
	operationRecoveryReason map[string]string
}

// reconcileDashboardAgents gives each Agent one worker slot and one deadline
// shared by project and operation recovery. Dashboard latency is therefore
// bounded by a single per-Agent budget rather than one budget per capability.
func (b *Backend) reconcileDashboardAgents(ctx context.Context, agents []agentRow) (dashboardReconcileResult, error) {
	type agentResult struct {
		projectErr          error
		operationErr        error
		recoveryUnsupported bool
	}
	results := make([]agentResult, len(agents))
	jobs := make(chan int)
	var wait sync.WaitGroup
	workers := min(len(agents), maxConcurrentHostProbes)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				session, ok := b.registry.Current(agents[index].id)
				if !ok || session.State() != producttransport.StateActive {
					continue
				}
				agentCtx, cancel := context.WithTimeout(ctx, dashboardAgentReconcileTimeout)
				results[index].projectErr = b.syncAgentProjects(agentCtx, agents[index].id, session)
				if results[index].projectErr != nil {
					results[index].operationErr = fmt.Errorf("%w: project identity reconciliation did not succeed", webui.ErrConflict)
					cancel()
					continue
				}
				recovery, ok := session.(producttransport.OperationRecoverySession)
				if !ok {
					results[index].recoveryUnsupported = true
				} else if agentCtx.Err() != nil {
					results[index].operationErr = agentCtx.Err()
				} else {
					results[index].operationErr = b.reconcileAgentOperations(agentCtx, agents[index].id, recovery)
				}
				cancel()
			}
		}()
	}
	for index := range agents {
		jobs <- index
	}
	close(jobs)
	wait.Wait()

	result := dashboardReconcileResult{
		discoveryReasons:        make(map[string]string),
		operationRecoveryReason: make(map[string]string),
	}
	for index, state := range results {
		agentID := agents[index].id
		if state.projectErr != nil {
			switch {
			case ctx.Err() != nil:
				return dashboardReconcileResult{}, ctx.Err()
			case errors.Is(state.projectErr, ErrCorruptData):
				result.discoveryReasons[agentID] = "project discovery response is invalid"
			case errors.Is(state.projectErr, context.DeadlineExceeded), errors.Is(state.projectErr, webui.ErrUnavailable):
				result.discoveryReasons[agentID] = "project discovery is unavailable"
			default:
				return dashboardReconcileResult{}, state.projectErr
			}
		}
		if state.recoveryUnsupported {
			result.operationRecoveryReason[agentID] = "active operation recovery is unsupported by this Agent protocol"
			continue
		}
		if state.operationErr == nil {
			continue
		}
		switch {
		case ctx.Err() != nil:
			return dashboardReconcileResult{}, ctx.Err()
		case errors.Is(state.operationErr, producttransport.ErrHandlerUnavailable):
			result.operationRecoveryReason[agentID] = "active operation recovery is unsupported by this Agent protocol"
		case errors.Is(state.operationErr, producttransport.ErrProtocol), errors.Is(state.operationErr, ErrCorruptData):
			result.operationRecoveryReason[agentID] = "Agent active operation recovery response is invalid"
		case errors.Is(state.operationErr, webui.ErrConflict):
			result.operationRecoveryReason[agentID] = "Agent active operation metadata conflicts with Server history"
		case errors.Is(state.operationErr, context.DeadlineExceeded), errors.Is(state.operationErr, webui.ErrUnavailable):
			result.operationRecoveryReason[agentID] = "active operation recovery is unavailable"
		default:
			return dashboardReconcileResult{}, state.operationErr
		}
	}
	return result, nil
}
