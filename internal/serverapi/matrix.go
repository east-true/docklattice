package serverapi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/servermatrix"
	"github.com/east-true/dockpilot/internal/webui"
)

// metricsUnsupportedError is an Agent that cannot serve the metrics matrix at
// all, which is a property of how it was built rather than a passing failure.
//
// Its message is Server-authored and safe to show. The Agent's own capability
// reason is deliberately not interpolated here: capability reasons already have
// a place in the dashboard, and this text is rendered straight into an HTTP
// response body.
type metricsUnsupportedError struct{ agentID string }

func (e *metricsUnsupportedError) Error() string {
	return fmt.Sprintf("serverapi: Agent %q does not report the live metrics capability", e.agentID)
}

func (e *metricsUnsupportedError) Unwrap() []error { return []error{webui.ErrUnavailable} }

// matrixSessions opens one host's metrics stream, and owns the questions the
// fan-out deliberately does not ask: whether the Agent is connected, and
// whether it can serve metrics at all.
//
// The capability is read from a live heartbeat rather than the stored row.
// CurrentProductProtocolVersion cannot answer this - an Agent built before the
// feature reports the same version as one built after it - so the flag is the
// only thing that can, and an Agent that does not know the field leaves it
// false, which is exactly the answer needed.
type matrixSessions struct{ backend *Backend }

func (s matrixSessions) Open(ctx context.Context, agentID string) (servermatrix.FrameStream, error) {
	session, err := s.backend.activeSession(agentID)
	if err != nil {
		return nil, err
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, hostProbeTimeout)
	heartbeat, err := session.Heartbeat(probeCtx)
	cancelProbe()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: agentID, action: "metrics capability", cause: err}
	}
	if !heartbeat.Capability.MetricsMatrix {
		return nil, &metricsUnsupportedError{agentID: agentID}
	}
	// The stream itself is opened on the relay's context, not the probe's: it
	// lives until the last viewer leaves.
	stream, err := session.OpenMetricsMatrix(ctx, producttransport.MetricsMatrixRequest{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: agentID, action: "metrics", cause: err}
	}
	return stream, nil
}

// matrixContext answers what this Server knows about the containers on a host.
//
// It reads and never writes. Project identity is reconciled by the dashboard
// path, which owns the projects table; a metrics viewer must not become a
// second writer of it, or watching a host would start changing what the Server
// believes about that host.
type matrixContext struct{ backend *Backend }

func (c matrixContext) ContainerContext(ctx context.Context, agentID string) (map[string]servermatrix.ContainerContext, error) {
	facts, err := c.backend.dockerProjectFacts(ctx, agentID)
	if err != nil {
		return nil, err
	}
	// Images come from the container inventory because Compose labels do not
	// carry them. Both halves must succeed: unlike the two halves of a frame,
	// which describe different things and fail apart, these are one answer
	// about the same containers, and half of it presented as whole would be a
	// gap nothing reports.
	containers, err := c.backend.HostContainers(ctx, agentID)
	if err != nil {
		return nil, err
	}
	managed, err := c.backend.managedProjectUIDs(ctx, agentID)
	if err != nil {
		return nil, err
	}

	mapping := make(map[string]servermatrix.ContainerContext, len(containers))
	for _, fact := range facts {
		entry := servermatrix.ContainerContext{ProjectName: fact.ProjectName, Service: fact.Service}
		// A UID is claimed only for a project this Server actually manages.
		// Computing one for somebody else's Compose stack on the same host
		// would invent an identity that resolves to nothing; the project is
		// still shown, under the name its labels give it.
		if fact.WorkingDir != "" {
			if uid, err := projectmodel.UID(agentID, fact.WorkingDir); err == nil {
				if _, present := managed[uid]; present {
					entry.ProjectUID = uid
				}
			}
		}
		mapping[fact.ContainerID] = entry
	}
	for _, container := range containers {
		entry := mapping[container.ID]
		entry.Image = container.Image
		mapping[container.ID] = entry
	}
	return mapping, nil
}

// dockerProjectFacts reads the Compose labels the Agent observed, and nothing
// else from the discovery response. The project snapshots in the same payload
// belong to the reconciliation path.
func (b *Backend) dockerProjectFacts(ctx context.Context, agentID string) ([]projectmodel.DockerFact, error) {
	session, err := b.activeSession(agentID)
	if err != nil {
		return nil, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, hostInventoryTimeout)
	defer cancel()
	response, err := session.Query(queryCtx, producttransport.QueryRequest{Kind: QueryProjectList})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: agentID, action: "project discovery", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return nil, &corruptDataError{boundary: "Agent project discovery response", cause: errors.New("payload exceeds transport limit")}
	}
	var snapshot agentProjectList
	if err := decodeStrictJSON(response.Payload, &snapshot); err != nil {
		return nil, &corruptDataError{boundary: "Agent project discovery response", cause: err}
	}
	return validateDockerProjectFacts(snapshot.DockerFacts)
}

func (b *Backend) managedProjectUIDs(ctx context.Context, agentID string) (map[string]struct{}, error) {
	rows, err := b.store.DB().QueryContext(ctx, `SELECT project_uid FROM projects WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, fmt.Errorf("serverapi: list managed projects: %w", err)
	}
	defer rows.Close()
	uids := make(map[string]struct{})
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("serverapi: list managed projects: %w", err)
		}
		uids[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("serverapi: list managed projects: %w", err)
	}
	return uids, nil
}

// OpenMatrix attaches a viewer to one host's live metrics. Every viewer of a
// host shares one Agent stream; see internal/servermatrix.
func (b *Backend) OpenMatrix(ctx context.Context, agentID string) (webui.MatrixStream, error) {
	if err := b.authorizeAgent(ctx, agentID); err != nil {
		return nil, err
	}
	subscription, err := b.matrix.Subscribe(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &liveMatrixStream{subscription: subscription}, nil
}

// liveMatrixStream renames the Server's view into the browser's shape and does
// nothing else. Every judgement in a frame was made before it got here.
type liveMatrixStream struct {
	subscription *servermatrix.Subscription
	once         sync.Once
}

func (s *liveMatrixStream) Recv(ctx context.Context) (webui.MatrixFrame, error) {
	view, err := s.subscription.Next(ctx)
	if err != nil {
		_ = s.Close()
		return webui.MatrixFrame{}, err
	}
	return matrixFrameFromView(view), nil
}

func (s *liveMatrixStream) Close() error {
	var err error
	s.once.Do(func() { err = s.subscription.Close() })
	return err
}

func matrixFrameFromView(view servermatrix.View) webui.MatrixFrame {
	filesystems := make([]webui.MatrixFilesystem, 0, len(view.Host.Filesystems))
	for _, filesystem := range view.Host.Filesystems {
		filesystems = append(filesystems, webui.MatrixFilesystem{
			Path: filesystem.Path, TotalBytes: filesystem.TotalBytes, FreeBytes: filesystem.FreeBytes,
			Unavailable: filesystem.Unavailable, Reason: filesystem.Reason,
		})
	}
	projects := make([]webui.MatrixProject, 0, len(view.Projects))
	for _, project := range view.Projects {
		services := make([]webui.MatrixService, 0, len(project.Services))
		for _, service := range project.Services {
			containers := make([]webui.MatrixContainer, 0, len(service.Containers))
			for _, container := range service.Containers {
				containers = append(containers, matrixContainerFromRow(container))
			}
			services = append(services, webui.MatrixService{
				Service: service.Service, Unmapped: service.Unmapped,
				Totals: matrixTotals(service.Totals), Containers: containers,
			})
		}
		projects = append(projects, webui.MatrixProject{
			ProjectUID: project.ProjectUID, ProjectName: project.ProjectName, Unmapped: project.Unmapped,
			Totals: matrixTotals(project.Totals), Services: services,
		})
	}
	return webui.MatrixFrame{
		AgentID: view.AgentID, ObservedAt: view.ObservedAt,
		Host: webui.MatrixHostRow{
			CPUCapacity: view.Host.CPUCapacity, MemoryCapacity: view.Host.MemoryCapacity,
			ContainersRunning: view.Host.ContainersRunning, ContainersTotal: view.Host.ContainersTotal,
			Filesystems: filesystems, Totals: matrixTotals(view.Host.Totals),
		},
		Projects:           projects,
		AgentDroppedFrames: view.AgentDropped, ServerDroppedFrames: view.ViewerDropped,
		MembershipStale: view.MembershipStale, MembershipReason: view.MembershipReason,
		WorkloadStale: view.WorkloadStale, WorkloadReason: view.WorkloadReason,
		ContextStale: view.ContextStale, ContextReason: view.ContextReason,
	}
}

func matrixContainerFromRow(row servermatrix.ContainerRow) webui.MatrixContainer {
	return webui.MatrixContainer{
		ContainerID: row.ContainerID, Pending: row.Pending, Unmapped: row.Unmapped,
		ProjectUID: row.ProjectUID, ProjectName: row.ProjectName, Service: row.Service, Image: row.Image,
		Sample: webui.StatsSample{
			ContainerID: row.ContainerID, ObservedAt: row.Sample.ObservedAt, CPUPercent: row.Sample.CPUPercent,
			MemoryUsage: row.Sample.MemoryUsage, MemoryLimit: row.Sample.MemoryLimit,
			NetworkRX: row.Sample.NetworkRX, NetworkTX: row.Sample.NetworkTX,
			BlockRead: row.Sample.BlockRead, BlockWrite: row.Sample.BlockWrite,
			RestartCount: row.Sample.RestartCount, Health: row.Sample.Health, Uptime: row.Sample.Uptime,
		},
		MemoryLimitUnbounded: row.MemoryLimitUnbounded,
		MemoryPercent:        row.MemoryPercent, MemoryPercentKnown: row.MemoryPercentKnown,
	}
}

func matrixTotals(totals servermatrix.Aggregate) webui.MatrixTotals {
	return webui.MatrixTotals{
		ContainerCount: totals.ContainerCount, PendingCount: totals.PendingCount,
		CPUPercent: totals.CPUPercent, MemoryUsage: totals.MemoryUsage,
		NetworkRX: totals.NetworkRX, NetworkTX: totals.NetworkTX,
		BlockRead: totals.BlockRead, BlockWrite: totals.BlockWrite, Restarts: totals.Restarts,
		MemoryLimit: totals.MemoryLimit, MemoryLimitUnbounded: totals.MemoryLimitUnbounded,
		MemoryPercent: totals.MemoryPercent, MemoryPercentKnown: totals.MemoryPercentKnown,
		Health: totals.Health, HealthUnreported: totals.HealthUnreported,
		Uptime: totals.Uptime, UptimeKnown: totals.UptimeKnown,
	}
}

// Close releases what the Backend started on its own: the metrics relays and
// the Agent streams behind them. The store and the session registry belong to
// whoever opened them.
func (b *Backend) Close() error { return b.matrix.Close() }
