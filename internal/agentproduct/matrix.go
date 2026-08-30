package agentproduct

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/east-true/docklattice/internal/dockeradapter"
	"github.com/east-true/docklattice/internal/livematrix"
	"github.com/east-true/docklattice/internal/livestats"
)

// MatrixDocker is the Docker surface live metrics need. It is separate from
// Docker because the three calls fail independently and are used on different
// cadences: membership every reconcile, host capacity every periodic one, and
// events continuously.
type MatrixDocker interface {
	ListRunning(context.Context) ([]dockeradapter.Container, error)
	Info(context.Context) (dockeradapter.EngineInfo, error)
	SubscribeEvents(context.Context, time.Time) (dockeradapter.EventStream, error)
}

var _ MatrixDocker = (*dockeradapter.Adapter)(nil)

// matrixEventRetry is how long a broken event subscription waits before trying
// again. Events are an optimization - the periodic reconcile repairs whatever
// they miss - so this is deliberately unhurried.
const matrixEventRetry = 5 * time.Second

type dockerMembership struct{ docker MatrixDocker }

func (m dockerMembership) Running(ctx context.Context) ([]string, error) {
	containers, err := m.docker.ListRunning(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		if container.ID != "" {
			ids = append(ids, container.ID)
		}
	}
	return ids, nil
}

// dockerEvents turns Docker's container lifecycle events into a bare "ask
// again" signal. It reports no detail, because acting on the detail would mean
// two ways to learn membership that could disagree; there is one, and this only
// decides when to use it.
type dockerEvents struct {
	docker MatrixDocker
	retry  time.Duration
}

func (e dockerEvents) Watch(ctx context.Context, changed func()) error {
	retry := e.retry
	if retry <= 0 {
		retry = matrixEventRetry
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A subscription that has just been (re)established may have missed
		// transitions while it was down, so reconcile once on the way in rather
		// than waiting for the periodic repair.
		changed()
		e.consume(ctx, changed)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}

// consume returns when the current subscription ends for any reason. A dead
// event stream degrades the frame cadence, never the frame: membership still
// follows the periodic reconcile.
func (e dockerEvents) consume(ctx context.Context, changed func()) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := e.docker.SubscribeEvents(streamCtx, time.Time{})
	if err != nil {
		return
	}
	for {
		select {
		case <-streamCtx.Done():
			return
		case event, ok := <-stream.Events:
			if !ok {
				return
			}
			// Only containers change membership. Image, volume and network
			// events arrive on the same subscription and are discarded here so
			// that pulling an image does not cost a container listing.
			if event.ResourceType == "container" {
				changed()
			}
		case <-stream.Errors:
			return
		}
	}
}

// dockerWorkload is the host row: what the Engine says about the machine it
// runs on, plus capacity for the paths DockLattice writes to.
type dockerWorkload struct {
	docker MatrixDocker
	paths  []string
	probe  func(string) (filesystemUsage, error)
}

type filesystemUsage struct {
	Device     uint64
	TotalBytes uint64
	FreeBytes  uint64
}

func (w dockerWorkload) Capacity(ctx context.Context) (livematrix.Capacity, error) {
	info, err := w.docker.Info(ctx)
	if err != nil {
		return livematrix.Capacity{}, err
	}
	return livematrix.Capacity{
		CPUCapacity:     info.CPUCapacity,
		MemoryCapacity:  info.MemoryCapacity,
		ContainersTotal: info.ContainersTotal,
		Filesystems:     w.filesystems(),
	}, nil
}

// filesystems reports capacity for the configured paths, deduplicated by the
// filesystem they live on so two discovery roots under one mount appear once.
//
// It reports these paths and nothing else. It is not a mount inventory, and
// must not become one: the question is how much room DockLattice has where it
// writes, which is answered by the paths it writes to.
func (w dockerWorkload) filesystems() []livematrix.Filesystem {
	probe := w.probe
	if probe == nil {
		probe = probeFilesystem
	}
	filesystems := make([]livematrix.Filesystem, 0, len(w.paths))
	seen := make(map[uint64]struct{}, len(w.paths))
	for _, path := range w.paths {
		if path == "" {
			continue
		}
		usage, err := probe(path)
		if err != nil {
			// One unreadable path is a fact about that path. It is reported as
			// such and does not make the rest of the summary unavailable.
			filesystems = append(filesystems, livematrix.Filesystem{
				Path: path, Unavailable: true, Reason: boundedReason(err),
			})
			continue
		}
		if _, duplicate := seen[usage.Device]; duplicate {
			continue
		}
		seen[usage.Device] = struct{}{}
		filesystems = append(filesystems, livematrix.Filesystem{
			Path: path, TotalBytes: usage.TotalBytes, FreeBytes: usage.FreeBytes,
		})
	}
	return filesystems
}

// boundedReason keeps a reason short enough to travel in every frame.
func boundedReason(err error) string {
	const limit = 200
	message := err.Error()
	if message == "" {
		message = "filesystem is unavailable"
	}
	if len(message) > limit {
		message = message[:limit]
	}
	return message
}

func newMatrixHub(config Config, stats *livestats.Hub) (*livematrix.Hub, error) {
	if config.MatrixDocker == nil {
		return nil, errors.New("agentproduct: matrix Docker source is required")
	}
	interval := config.MatrixFrameInterval
	if interval <= 0 {
		return nil, fmt.Errorf("agentproduct: matrix frame interval must be positive")
	}
	return livematrix.New(livematrix.Config{
		Stats:         stats,
		Membership:    dockerMembership{docker: config.MatrixDocker},
		Events:        dockerEvents{docker: config.MatrixDocker, retry: config.matrixEventRetry},
		Workload:      dockerWorkload{docker: config.MatrixDocker, paths: config.MatrixPaths, probe: config.matrixProbe},
		FrameInterval: interval,
	})
}
