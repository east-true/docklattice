package dockeradapter

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// EngineInfo is the Engine's own account of the host it runs on.
//
// It exists because an Agent normally runs in a container, where /proc belongs
// to the container's namespaces and answers about the Agent rather than the
// host. The daemon does run on the host, so asking it is the one way to state
// host capacity that means the same thing in every deployment.
type EngineInfo struct {
	CPUCapacity       uint32
	MemoryCapacity    uint64
	ContainersTotal   uint32
	ContainersRunning uint32
}

// infoEngine is optional at the constructor boundary for the same reason
// inventoryEngine is: focused mutation fakes should not have to implement
// unrelated read APIs. The production Moby client implements it.
type infoEngine interface {
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
}

var _ infoEngine = (*client.Client)(nil)

// Info reports host capacity and the daemon's container counts. It is a
// capacity read, not an inventory: the container rows come from ListRunning,
// and the two are deliberately separate calls so one failing does not take the
// other with it.
func (adapter *Adapter) Info(ctx context.Context) (EngineInfo, error) {
	api, ok := adapter.engine.(infoEngine)
	if !ok {
		return EngineInfo{}, fmt.Errorf("%w: Engine client does not report host info", ErrUnavailable)
	}
	result, err := api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return EngineInfo{}, fmt.Errorf("%w: engine info: %v", ErrUnavailable, err)
	}
	return EngineInfo{
		CPUCapacity:       countAsUint32(result.Info.NCPU),
		MemoryCapacity:    countAsUint64(result.Info.MemTotal),
		ContainersTotal:   countAsUint32(result.Info.Containers),
		ContainersRunning: countAsUint32(result.Info.ContainersRunning),
	}, nil
}

// ListRunning lists only running containers. List reports every container
// because inventory and audit care about stopped ones; live metrics do not,
// and asking the Engine to filter avoids carrying the stopped set across the
// socket on every reconcile.
func (adapter *Adapter) ListRunning(ctx context.Context) ([]Container, error) {
	result, err := adapter.engine.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: list running containers: %v", ErrUnavailable, err)
	}
	containers := make([]Container, 0, len(result.Items))
	for _, item := range result.Items {
		containers = append(containers, fromSummary(item))
	}
	return containers, nil
}

// countAsUint32 and countAsUint64 drop negative counts rather than wrapping
// them. A negative CPU count is not a large one.
func countAsUint32(value int) uint32 {
	if value <= 0 {
		return 0
	}
	return uint32(value)
}

func countAsUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}
