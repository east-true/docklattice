package serverapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
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
	// The gate is what stops the RPC, not what cleans up after it. An Agent
	// built before this feature must never be sent a call it does not
	// implement, whatever it would answer.
	if got := session.matrixOpenCalls(); got != 0 {
		t.Fatalf("the matrix RPC was sent %d times to an Agent without the capability", got)
	}

	session.mu.Lock()
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.mu.Unlock()
	viewer, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("open matrix with the capability: %v", err)
	}
	if got := session.matrixOpenCalls(); got != 1 {
		t.Fatalf("the matrix RPC was sent %d times once the capability was reported, want once", got)
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
	view, err := viewer.Recv(viewCtx)
	if err != nil {
		t.Fatalf("next frame: %v", err)
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
}

// Viewer lifecycle at the Backend boundary: one host stream, shared while
// anyone is watching, ended by the last viewer to leave.
func TestMatrixAgentStreamEndsWithTheLastViewer(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.queryPayload = []byte(`[]`)
	stream := newFakeMatrixStream()
	session.matrixStream = stream
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	first, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("first viewer: %v", err)
	}
	second, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("second viewer: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first viewer: %v", err)
	}
	select {
	case <-stream.closed:
		t.Fatal("the Agent stream ended while a viewer was still watching")
	case <-time.After(50 * time.Millisecond):
	}

	if err := second.Close(); err != nil {
		t.Fatalf("close second viewer: %v", err)
	}
	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the Agent stream outlived its last viewer")
	}
}

// The Agent's drop count and the Server's reach the browser as two numbers.
//
// What is asserted here is the accounting, not a particular amount of
// coalescing: whether a given round is delivered or dropped depends on when the
// reader arrives, and pinning that number would be testing the scheduler. The
// invariant that must hold either way is that every round is accounted for
// exactly once - delivered or counted as dropped, never both and never
// neither - and that the Agent's own count is not folded into it. The
// deterministic coalescing behaviour has its own test in internal/servermatrix,
// where the pushes can be sequenced.
func TestMatrixDropCountersReachTheFrameSeparately(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.queryPayload = []byte(`[]`)
	stream := newFakeMatrixStream()
	session.matrixStream = stream
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	viewer, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("open matrix: %v", err)
	}
	defer viewer.Close()

	const rounds = 3
	for round := 0; round < rounds; round++ {
		stream.frames <- producttransport.MetricsMatrixFrame{
			ObservedAt: time.Unix(int64(round), 0).UTC(), DroppedFrames: 4,
			Workload: producttransport.WorkloadSummary{CPUCapacity: 2},
		}
	}

	delivered, dropped := 0, uint64(0)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && delivered+int(dropped) < rounds {
		recvCtx, cancel := context.WithTimeout(ctx, time.Second)
		frame, err := viewer.Recv(recvCtx)
		cancel()
		if err != nil {
			t.Fatalf("recv frame: %v", err)
		}
		delivered++
		dropped = frame.ServerDroppedFrames
		// The Agent's own count travels on every frame and is never merged
		// into the Server's; folding them together would hide which side of
		// the stream is behind.
		if frame.AgentDroppedFrames != 4 {
			t.Fatalf("the Agent's drop count arrived as %d, want 4 kept separate", frame.AgentDroppedFrames)
		}
	}
	if delivered+int(dropped) != rounds {
		t.Fatalf("%d rounds delivered and %d counted as dropped, which does not account for the %d sent",
			delivered, dropped, rounds)
	}
}

// The capability answer a console reads before opening a stream lives where
// every other capability reason already lives.
func TestDashboardReportsTheMetricsCapability(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	host, err := backend.Host(ctx, "agent-a")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if host.Capabilities.Metrics.Enabled {
		t.Fatal("an Agent that does not report the capability was shown as able to serve metrics")
	}
	if host.Capabilities.Metrics.Reason == "" {
		t.Fatal("the disabled metrics capability carries no reason")
	}

	session.mu.Lock()
	session.capability.MetricsMatrix = true
	session.mu.Unlock()
	host, err = backend.Host(ctx, "agent-a")
	if err != nil {
		t.Fatalf("host after capability: %v", err)
	}
	if !host.Capabilities.Metrics.Enabled {
		t.Fatal("an Agent reporting the capability was still shown as unable")
	}
}

// The shutdown order the runtime depends on, exercised end to end over a real
// listener: closing the Backend ends the relays, which unblocks the SSE
// handlers that were reading them, so HTTP shutdown drains instead of waiting
// out its deadline on streams that were never going to end on their own.
func TestBackendCloseUnblocksStreamingHandlersBeforeShutdown(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{MetricsMatrix: true}
	session.queryPayload = []byte(`[]`)
	stream := newFakeMatrixStream()
	session.matrixStream = stream
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	handler, err := webui.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()

	client := &http.Client{}
	defer client.CloseIdleConnections()
	baseline := runtime.NumGoroutine()

	response, err := client.Get("http://" + listener.Addr().String() + "/api/v1/live/matrix?agent_id=agent-a")
	if err != nil {
		t.Fatalf("open matrix stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("matrix stream opened with %d", response.StatusCode)
	}
	// Read past the opening comment so the handler is certainly parked in Recv
	// with nothing to write.
	opening := make([]byte, len(": stream-open\n\n"))
	if _, err := io.ReadFull(response.Body, opening); err != nil {
		t.Fatalf("read stream open: %v", err)
	}
	stream.frames <- producttransport.MetricsMatrixFrame{Workload: producttransport.WorkloadSummary{CPUCapacity: 1}}
	buffer := make([]byte, 1)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatalf("read first frame: %v", err)
	}

	if err := backend.Close(); err != nil {
		t.Fatalf("close backend: %v", err)
	}
	// The handler must return on its own now. Draining the body is how a
	// caller sees that: it ends rather than hanging until a deadline.
	bodyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		bodyDone <- err
	}()
	select {
	case <-bodyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the SSE handler kept the response open after the Backend closed")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	started := time.Now()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown took %v, which means it waited on a stream rather than draining", elapsed)
	}
	<-serveDone

	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the Agent stream was left open after shutdown")
	}

	client.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutines settled at %d, above the %d before the stream opened", runtime.NumGoroutine(), baseline)
}

// An Agent upgraded to a build that serves metrics becomes watchable when it
// reconnects. The Server must not have cached "this host cannot do metrics"
// from the session before it.
func TestMatrixFollowsAnAgentUpgradeAcrossReconnect(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")

	older := newFakeSession("agent-a")
	older.queryPayload = []byte(`[]`)
	older.matrixStream = newFakeMatrixStream()
	if err := registry.Register(older); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.OpenMatrix(ctx, "agent-a"); !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("the pre-upgrade Agent answered %v, want unavailable", err)
	}
	if got := older.matrixOpenCalls(); got != 0 {
		t.Fatalf("the older Agent was sent %d matrix RPCs", got)
	}

	upgraded := newFakeSession("agent-a")
	upgraded.info.SessionID = "session-agent-a-upgraded"
	upgraded.info.Incarnation = 2
	upgraded.capability = producttransport.Capability{MetricsMatrix: true}
	upgraded.queryPayload = []byte(`[]`)
	upgraded.matrixStream = newFakeMatrixStream()
	if err := registry.Register(upgraded); err != nil {
		t.Fatal(err)
	}

	viewer, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("open matrix after the upgrade: %v", err)
	}
	defer viewer.Close()
	if got := upgraded.matrixOpenCalls(); got != 1 {
		t.Fatalf("the upgraded Agent was sent %d matrix RPCs, want one", got)
	}
	if got := older.matrixOpenCalls(); got != 0 {
		t.Fatalf("the replaced session was still being called %d times", got)
	}
}

// A viewer watching a host when its Agent reconnects is released with the
// failure rather than left holding a stream that will never produce another
// frame. Reopening then lands on the new session.
func TestMatrixViewerIsReleasedWhenTheAgentSessionIsReplaced(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	t.Cleanup(func() { _ = backend.Close() })
	insertAgent(t, ctx, store, "agent-a", "Host A", "{}")

	first := newFakeSession("agent-a")
	first.capability = producttransport.Capability{MetricsMatrix: true}
	first.queryPayload = []byte(`[]`)
	first.matrixStream = newFakeMatrixStream()
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	viewer, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("open matrix: %v", err)
	}
	defer viewer.Close()

	replacement := newFakeSession("agent-a")
	replacement.info.SessionID = "session-agent-a-second"
	replacement.info.Incarnation = 2
	replacement.capability = producttransport.Capability{MetricsMatrix: true}
	replacement.queryPayload = []byte(`[]`)
	replacement.matrixStream = newFakeMatrixStream()
	if err := registry.Register(replacement); err != nil {
		t.Fatal(err)
	}

	recvCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := viewer.Recv(recvCtx); err == nil {
		t.Fatal("the viewer kept waiting on a stream whose session was replaced")
	}

	next, err := backend.OpenMatrix(ctx, "agent-a")
	if err != nil {
		t.Fatalf("reopen matrix on the new session: %v", err)
	}
	defer next.Close()
	if got := replacement.matrixOpenCalls(); got != 1 {
		t.Fatalf("the new session was sent %d matrix RPCs, want one", got)
	}
}
