package serverapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/servermatrix"
	"github.com/east-true/dockpilot/internal/webui"
)

type fakeMatrixReceiveStream struct {
	frames chan producttransport.MetricsMatrixFrame
	closed chan struct{}
}

func newFakeMatrixStream() *fakeMatrixReceiveStream {
	return &fakeMatrixReceiveStream{frames: make(chan producttransport.MetricsMatrixFrame, 4), closed: make(chan struct{})}
}

func (s *fakeMatrixReceiveStream) Recv(ctx context.Context) (producttransport.MetricsMatrixFrame, error) {
	select {
	case frame := <-s.frames:
		return frame, nil
	case <-s.closed:
		return producttransport.MetricsMatrixFrame{}, errors.New("stream closed")
	case <-ctx.Done():
		return producttransport.MetricsMatrixFrame{}, ctx.Err()
	}
}

func (s *fakeMatrixReceiveStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func matrixContainerID(letter string) string { return strings.Repeat(letter, 64) }

// An Agent built before this feature reports the same protocol version as one
// built after it, so the capability flag is the only thing that can tell them
// apart. A host without it is refused with a reason a browser can show, not
// with an empty stream.
func TestOpenMatrixRequiresTheReportedCapability(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.matrixStream = newFakeMatrixStream()
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	_, err := backend.OpenMatrix(ctx, "agent-a")
	if !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("open matrix without the capability returned %v, want an unavailable capability", err)
	}
	if !strings.Contains(err.Error(), "live metrics capability") {
		t.Fatalf("error %q does not say what is missing", err)
	}
	if session.heartbeatCalls() == 0 {
		t.Fatal("the capability was not read from a live heartbeat")
	}

	session.mu.Lock()
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.mu.Unlock()
	viewer, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("open matrix with the capability: %v", err)
	}
	_ = viewer.Close()
}

func TestOpenMatrixRejectsAnAgentTheServerDoesNotKnow(t *testing.T) {
	ctx, backend, _, _ := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	if _, err := backend.OpenMatrix(ctx, "agent-missing"); !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("open matrix for an unknown Agent returned %v, want not found", err)
	}
	if _, err := backend.OpenMatrix(ctx, ""); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("open matrix without an Agent ID returned %v, want an invalid request", err)
	}
}

// The end-to-end join: Compose labels give project and service, the container
// inventory gives images, and the projects table decides which project names
// are identities this Server can resolve.
func TestMatrixViewJoinsDiscoveryContext(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	const agentID = "11111111-1111-4111-8111-111111111111"
	insertAgent(t, ctx, store, agentID, "Host A", "{}")

	managedUID, err := projectmodel.UID(agentID, "/srv/shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO projects(project_uid, agent_id, working_dir, name, flags_json, updated_at)
		VALUES (?, ?, ?, ?, '{}', ?)
	`, managedUID, agentID, "/srv/shop", "shop", dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}

	web, api, loose := matrixContainerID("a"), matrixContainerID("b"), matrixContainerID("c")
	session := newFakeSession(agentID)
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.projectListPayload = []byte(fmt.Sprintf(`{
		"projects": [],
		"docker_facts": [
			{"container_id": %q, "project_name": "shop", "working_dir": "/srv/shop", "service": "web"},
			{"container_id": %q, "project_name": "borrowed", "working_dir": "/opt/borrowed", "service": "api"}
		],
		"status": {"scanned_at": "2026-08-15T00:00:00Z", "truncated": false, "directories_seen": 1}
	}`, web, api))
	session.queryPayload = []byte(fmt.Sprintf(`[
		{"id": %q, "names": ["/shop-web"], "image": "nginx:1", "state": "running", "status": "Up", "mounts": []},
		{"id": %q, "names": ["/borrowed-api"], "image": "api:2", "state": "running", "status": "Up", "mounts": []},
		{"id": %q, "names": ["/scratch"], "image": "redis:7", "state": "running", "status": "Up", "mounts": []}
	]`, web, api, loose))
	stream := newFakeMatrixStream()
	session.matrixStream = stream
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	viewer, err := backend.OpenMatrix(ctx, agentID)
	if err != nil {
		t.Fatalf("open matrix: %v", err)
	}
	defer viewer.Close()

	stream.frames <- producttransport.MetricsMatrixFrame{
		ObservedAt: time.Unix(2, 0).UTC(),
		Workload:   producttransport.WorkloadSummary{CPUCapacity: 4, MemoryCapacity: 8 << 30, ContainersRunning: 3, ContainersTotal: 3},
		Containers: []producttransport.StatsSample{
			{ContainerID: web, CPUPercent: 1, MemoryUsage: 100, MemoryLimit: 400},
			{ContainerID: api, CPUPercent: 2, MemoryUsage: 200, MemoryLimit: 400},
			{ContainerID: loose, CPUPercent: 4, MemoryUsage: 400},
		},
	}

	viewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	view, err := viewer.Next(viewCtx)
	if err != nil {
		t.Fatalf("next view: %v", err)
	}
	if view.ContextStale {
		t.Fatalf("context was reported stale: %q", view.ContextReason)
	}
	if len(view.Projects) != 3 {
		t.Fatalf("view has %d projects, want borrowed, shop, and the unmapped bucket: %+v", len(view.Projects), view.Projects)
	}

	borrowed, shop, unmapped := view.Projects[0], view.Projects[1], view.Projects[2]
	// A Compose project this Server does not manage keeps its name and gets no
	// UID, because a UID it does not hold a row for would resolve to nothing.
	if borrowed.ProjectName != "borrowed" || borrowed.ProjectUID != "" || borrowed.Unmapped {
		t.Fatalf("the unmanaged project is %+v", borrowed)
	}
	if shop.ProjectName != "shop" || shop.ProjectUID != managedUID {
		t.Fatalf("the managed project is %+v, want UID %q", shop, managedUID)
	}
	if !unmapped.Unmapped || len(unmapped.Services) != 1 {
		t.Fatalf("the unmapped bucket is %+v", unmapped)
	}

	row := shop.Services[0].Containers[0]
	if shop.Services[0].Service != "web" || row.Image != "nginx:1" || row.ContainerID != web {
		t.Fatalf("the shop web row is %+v", row)
	}
	if !row.MemoryPercentKnown || row.MemoryPercent != 25 {
		t.Fatalf("bounded memory percent is %v (known=%v)", row.MemoryPercent, row.MemoryPercentKnown)
	}
	scratch := unmapped.Services[0].Containers[0]
	if scratch.ContainerID != loose || scratch.Image != "redis:7" || !scratch.MemoryLimitUnbounded {
		t.Fatalf("the hand-started container is %+v, want its image and an unbounded limit", scratch)
	}
	if view.Host.Totals.ContainerCount != 3 || view.Host.Totals.CPUPercent != 7 {
		t.Fatalf("the host row is %+v, want all three containers", view.Host.Totals)
	}
	if view.Host.Totals.MemoryLimitUnbounded != true || view.Host.Totals.MemoryPercentKnown {
		t.Fatalf("one unlimited container did not make the host row unbounded: %+v", view.Host.Totals)
	}
}

// Two viewers of one host share one Agent stream, which is the whole reason
// this path exists.
func TestOpenMatrixSharesOneAgentStream(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.queryPayload = []byte(`[]`)
	session.matrixStream = newFakeMatrixStream()
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	first, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("first viewer: %v", err)
	}
	defer first.Close()
	heartbeatsAfterFirst := session.heartbeatCalls()
	second, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("second viewer: %v", err)
	}
	defer second.Close()
	if got := session.heartbeatCalls(); got != heartbeatsAfterFirst {
		t.Fatalf("the second viewer probed the Agent again (%d heartbeats, was %d)", got, heartbeatsAfterFirst)
	}
	var _ *servermatrix.Subscription = second
}
