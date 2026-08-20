package serverapi

import (
	"context"
	"errors"
	"fmt"

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
func (b *Backend) OpenMatrix(ctx context.Context, agentID string) (*servermatrix.Subscription, error) {
	if err := b.authorizeAgent(ctx, agentID); err != nil {
		return nil, err
	}
	return b.matrix.Subscribe(ctx, agentID)
}

// Close releases what the Backend started on its own: the metrics relays and
// the Agent streams behind them. The store and the session registry belong to
// whoever opened them.
func (b *Backend) Close() error { return b.matrix.Close() }
