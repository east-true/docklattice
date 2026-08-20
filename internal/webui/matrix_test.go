package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sliceMatrixStream struct {
	frames []MatrixFrame
	closed bool
}

func (s *sliceMatrixStream) Recv(context.Context) (MatrixFrame, error) {
	if len(s.frames) == 0 {
		return MatrixFrame{}, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

func (s *sliceMatrixStream) Close() error { s.closed = true; return nil }

type blockingMatrixStream struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (s *blockingMatrixStream) Recv(ctx context.Context) (MatrixFrame, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return MatrixFrame{}, ctx.Err()
}

func (s *blockingMatrixStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func matrixEventPayload(t *testing.T, body string) map[string]any {
	t.Helper()
	_, data, found := strings.Cut(body, "event: matrix\ndata: ")
	if !found {
		t.Fatalf("body has no matrix event: %q", body)
	}
	data, _, _ = strings.Cut(data, "\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("decode matrix event %q: %v", data, err)
	}
	return decoded
}

// A host that cannot be watched answers with a status and a reason before any
// SSE body is written. 500 is reserved for the Server accusing itself, and a
// missing capability is not that.
func TestMatrixCapabilityGapIsNotAServerFailure(t *testing.T) {
	backend := &testBackend{matrixErr: fmt.Errorf("%w: this Agent does not report the live metrics capability", ErrUnavailable)}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("capability gap answered %d, want 503: %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "CAPABILITY_UNAVAILABLE") {
		t.Fatalf("response carries no machine-readable code: %q", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "event: matrix") {
		t.Fatal("a refused host still opened a stream body")
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("refusal content type is %q, want a problem document", got)
	}
}

// An offline Agent is refused, not answered with an empty matrix. A 200
// carrying no containers says the host is idle, which is a different and wrong
// thing.
func TestMatrixOfflineAgentIsRefusedRatherThanShownEmpty(t *testing.T) {
	backend := &testBackend{matrixErr: fmt.Errorf("%w: agent offline", ErrUnavailable)}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent", nil))

	if response.Code == http.StatusOK {
		t.Fatalf("an offline Agent answered 200: %q", response.Body.String())
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "offline") {
		t.Fatalf("offline response = %d %q, want 503 saying it is offline", response.Code, response.Body.String())
	}
}

func TestMatrixUnknownAgentAndBadQueryAreClientErrors(t *testing.T) {
	backend := &testBackend{matrixErr: fmt.Errorf("%w: Agent is not in the Server cache", ErrNotFound)}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=ghost", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown Agent answered %d, want 404", response.Code)
	}

	for _, target := range []string{
		"/api/v1/live/matrix",
		"/api/v1/live/matrix?agent_id=",
		"/api/v1/live/matrix?agent_id=a&agent_id=b",
		"/api/v1/live/matrix?agent_id=a&container_id=" + strings.Repeat("b", 64),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s answered %d, want 400", target, response.Code)
		}
	}

	// The frame is the whole host, so there is nothing to resume into.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent", nil)
	request.Header.Set("Last-Event-ID", "7")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a resume attempt answered %d, want 400", response.Code)
	}
}

// Everything the Server decided about a frame reaches the browser unchanged.
// The HTTP layer normalizing any of it - filling in a reason, hiding a pending
// container, merging the drop counters - would contradict what was measured.
func TestMatrixFrameSemanticsSurviveEncoding(t *testing.T) {
	frame := MatrixFrame{
		AgentID:    "agent-1",
		ObservedAt: time.Unix(1700000000, 0).UTC(),
		Host: MatrixHostRow{
			CPUCapacity: 4, MemoryCapacity: 8 << 30, ContainersRunning: 2, ContainersTotal: 3,
			Filesystems: []MatrixFilesystem{
				{Path: "/srv", TotalBytes: 100, FreeBytes: 40},
				{Path: "/gone", Unavailable: true, Reason: "no such directory"},
			},
			Totals: MatrixTotals{ContainerCount: 2, PendingCount: 1, MemoryLimitUnbounded: true, Health: "healthy", HealthUnreported: 1},
		},
		Projects: []MatrixProject{{
			Unmapped: true,
			Totals:   MatrixTotals{ContainerCount: 2, PendingCount: 1},
			Services: []MatrixService{{
				Unmapped: true,
				Containers: []MatrixContainer{
					{ContainerID: "a", Unmapped: true, MemoryLimitUnbounded: true},
					{ContainerID: "b", Pending: true, Unmapped: true},
				},
			}},
		}},
		AgentDroppedFrames: 3, ServerDroppedFrames: 7,
		MembershipStale: true, MembershipReason: "docker listing failed",
		WorkloadStale: true, WorkloadReason: "engine info failed",
		ContextStale: true, ContextReason: "agent offline",
	}
	backend := &testBackend{matrixStream: &sliceMatrixStream{frames: []MatrixFrame{frame}}}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent-1", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("matrix SSE = %d %q", response.Code, response.Body.String())
	}
	if backend.matrixAgentID != "agent-1" {
		t.Fatalf("the backend was asked for host %q", backend.matrixAgentID)
	}
	decoded := matrixEventPayload(t, response.Body.String())

	// The three staleness facts move independently and none is invented or
	// merged on the way out.
	for field, want := range map[string]any{
		"membership_stale": true, "membership_reason": "docker listing failed",
		"workload_stale": true, "workload_reason": "engine info failed",
		"context_stale": true, "context_reason": "agent offline",
	} {
		if decoded[field] != want {
			t.Fatalf("%s encoded as %v, want %v", field, decoded[field], want)
		}
	}
	// The two drop counters stay apart, because they blame different sides.
	if decoded["agent_dropped_frames"] != float64(3) || decoded["server_dropped_frames"] != float64(7) {
		t.Fatalf("drop counters encoded as agent=%v server=%v, want 3 and 7",
			decoded["agent_dropped_frames"], decoded["server_dropped_frames"])
	}

	host := decoded["host"].(map[string]any)
	filesystems := host["filesystems"].([]any)
	if len(filesystems) != 2 || filesystems[1].(map[string]any)["unavailable"] != true {
		t.Fatalf("an unavailable filesystem was normalized away: %v", filesystems)
	}
	totals := host["totals"].(map[string]any)
	if totals["memory_limit_unbounded"] != true {
		t.Fatal("an unbounded memory limit did not survive encoding")
	}
	if _, present := totals["memory_limit"]; present {
		t.Fatal("an unbounded row still carried a memory_limit number")
	}
	if totals["health"] != "healthy" || totals["health_unreported"] != float64(1) {
		t.Fatalf("health encoded as %v with %v unreported", totals["health"], totals["health_unreported"])
	}
	if totals["pending_count"] != float64(1) {
		t.Fatalf("pending count encoded as %v", totals["pending_count"])
	}

	containers := decoded["projects"].([]any)[0].(map[string]any)["services"].([]any)[0].(map[string]any)["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("a pending or unmapped container was dropped: %v", containers)
	}
	if containers[1].(map[string]any)["pending"] != true || containers[1].(map[string]any)["unmapped"] != true {
		t.Fatalf("pending and unmapped did not survive encoding: %v", containers[1])
	}
}

// A browser that walks away releases the subscription, which is what lets the
// Agent stream end when it was the last viewer.
func TestMatrixViewerDisconnectClosesTheSubscription(t *testing.T) {
	stream := &blockingMatrixStream{started: make(chan struct{}), closed: make(chan struct{})}
	backend := &testBackend{matrixStream: stream}
	handler := newTestHandler(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent-1", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-stream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never read from the stream")
	}
	cancel()
	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the subscription was not closed when the viewer disconnected")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the viewer disconnected")
	}
}

// The handler holds no queue of its own: it writes whatever Recv hands it and
// asks for the next one. Coalescing belongs to the subscription, which is the
// only place that can count what it dropped.
func TestMatrixHandlerKeepsNoBacklogOfItsOwn(t *testing.T) {
	frames := make([]MatrixFrame, 3)
	for index := range frames {
		frames[index] = MatrixFrame{AgentID: "agent-1", ServerDroppedFrames: uint64(index * 5)}
	}
	backend := &testBackend{matrixStream: &sliceMatrixStream{frames: frames}}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/live/matrix?agent_id=agent-1", nil))

	body := response.Body.String()
	if got := strings.Count(body, "event: matrix"); got != 3 {
		t.Fatalf("wrote %d events for 3 frames", got)
	}
	if !strings.Contains(body, `"server_dropped_frames":10`) {
		t.Fatalf("the subscription's own drop count was not passed through: %q", body)
	}
	if stream := backend.matrixStream.(*sliceMatrixStream); !stream.closed {
		t.Fatal("the subscription was not closed when the stream ended")
	}
}

func TestMatrixRouteRejectsOtherMethods(t *testing.T) {
	handler := newTestHandler(t, &testBackend{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/api/v1/live/matrix?agent_id=agent", nil))
		if response.Code == http.StatusOK {
			t.Fatalf("%s on the matrix route answered 200", method)
		}
	}
}
