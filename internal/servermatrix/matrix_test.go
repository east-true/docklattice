package servermatrix

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
)

// fakeSessions is the transport as this package sees it: a counted Open and a
// stream that hands over whatever a test pushes into it.
type fakeSessions struct {
	mu     sync.Mutex
	opens  int
	err    error
	closed int
	stream *fakeStream
}

func (s *fakeSessions) Open(ctx context.Context, agentID string) (FrameStream, error) {
	s.mu.Lock()
	s.opens++
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	stream := &fakeStream{sessions: s, frames: make(chan producttransport.MetricsMatrixFrame, 16), done: make(chan struct{})}
	s.mu.Lock()
	s.stream = stream
	s.mu.Unlock()
	return stream, nil
}

func (s *fakeSessions) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

func (s *fakeSessions) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSessions) current() *fakeStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

type fakeStream struct {
	sessions *fakeSessions
	frames   chan producttransport.MetricsMatrixFrame

	mu     sync.Mutex
	err    error
	closed bool
	done   chan struct{}
}

func (s *fakeStream) Recv(ctx context.Context) (producttransport.MetricsMatrixFrame, error) {
	select {
	case frame := <-s.frames:
		return frame, nil
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return producttransport.MetricsMatrixFrame{}, err
	case <-ctx.Done():
		return producttransport.MetricsMatrixFrame{}, ctx.Err()
	}
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.sessions.mu.Lock()
		s.sessions.closed++
		s.sessions.mu.Unlock()
	}
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		close(s.done)
	}
	s.mu.Unlock()
}

func (s *fakeStream) push(frame producttransport.MetricsMatrixFrame) { s.frames <- frame }

func newTestHub(t *testing.T, sessions Sessions) *Hub {
	t.Helper()
	hub, err := New(Config{Sessions: sessions})
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub
}

func sampleFrame(ids ...string) producttransport.MetricsMatrixFrame {
	frame := producttransport.MetricsMatrixFrame{
		ObservedAt: time.Unix(1, 0).UTC(),
		Workload: producttransport.WorkloadSummary{
			CPUCapacity: 4, MemoryCapacity: 8 << 30,
			ContainersRunning: uint32(len(ids)), ContainersTotal: uint32(len(ids)),
		},
	}
	for _, id := range ids {
		frame.Containers = append(frame.Containers, producttransport.StatsSample{ContainerID: id, CPUPercent: 1})
	}
	return frame
}

func nextView(t *testing.T, viewer *Subscription) View {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	view, err := viewer.Next(ctx)
	if err != nil {
		t.Fatalf("next view: %v", err)
	}
	return view
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One Agent stream serves every browser watching that host, and the last
// viewer leaving ends it. This is the whole point of the package: N browsers
// must not become N streams to one Agent.
func TestOneAgentStreamServesEveryViewer(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	ctx := context.Background()

	first, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	second, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	if got := sessions.openCount(); got != 1 {
		t.Fatalf("opened %d Agent streams for two viewers, want 1", got)
	}

	sessions.current().push(sampleFrame("a", "b"))
	for name, viewer := range map[string]*Subscription{"first": first, "second": second} {
		view := nextView(t, viewer)
		if len(view.Containers) != 2 || view.Containers[0].ContainerID != "a" || view.Containers[1].ContainerID != "b" {
			t.Fatalf("%s viewer saw %+v, want both containers in ID order", name, view.Containers)
		}
		if view.AgentID != "agent-1" {
			t.Fatalf("%s viewer saw Agent %q", name, view.AgentID)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	if got := sessions.closeCount(); got != 0 {
		t.Fatalf("the Agent stream closed while a viewer was still watching")
	}
	sessions.current().push(sampleFrame("a", "b", "c"))
	if view := nextView(t, second); len(view.Containers) != 3 {
		t.Fatalf("the remaining viewer stopped receiving after the other left: %+v", view.Containers)
	}

	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
	waitFor(t, "the Agent stream to close with the last viewer", func() bool { return sessions.closeCount() == 1 })
}

// A separate host is a separate relay. Sharing is per host, not global.
func TestEachHostGetsItsOwnStream(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	ctx := context.Background()

	if _, err := hub.Subscribe(ctx, "agent-1"); err != nil {
		t.Fatalf("subscribe agent-1: %v", err)
	}
	if _, err := hub.Subscribe(ctx, "agent-2"); err != nil {
		t.Fatalf("subscribe agent-2: %v", err)
	}
	if got := sessions.openCount(); got != 2 {
		t.Fatalf("opened %d streams for two hosts, want 2", got)
	}
}

// A host that cannot be watched says so when the viewer subscribes. Returning
// a subscription that never produces anything would leave the browser unable
// to tell "no containers" from "this Agent cannot do metrics".
func TestSubscribeReportsWhyAHostCannotBeWatched(t *testing.T) {
	unsupported := errors.New("agent does not report the metrics capability")
	sessions := &fakeSessions{err: unsupported}
	hub := newTestHub(t, sessions)

	if _, err := hub.Subscribe(context.Background(), "agent-1"); !errors.Is(err, unsupported) {
		t.Fatalf("subscribe error was %v, want the reason the session source gave", err)
	}
	// The failed relay must not be left registered, or the host could never be
	// watched again without restarting the Server.
	sessions.mu.Lock()
	sessions.err = nil
	sessions.mu.Unlock()
	if _, err := hub.Subscribe(context.Background(), "agent-1"); err != nil {
		t.Fatalf("second subscribe after a failed open: %v", err)
	}
	if got := sessions.openCount(); got != 2 {
		t.Fatalf("opened %d streams, want the failed one to have been retried", got)
	}
}

// Viewers that arrive while the first open is still in flight wait for it
// rather than starting a second stream to the same Agent.
func TestConcurrentSubscribersShareOneOpen(t *testing.T) {
	release := make(chan struct{})
	sessions := &fakeSessions{}
	blocking := &blockingSessions{inner: sessions, release: release}
	hub := newTestHub(t, blocking)

	const viewers = 8
	var wait sync.WaitGroup
	results := make([]error, viewers)
	for index := 0; index < viewers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[index] = hub.Subscribe(context.Background(), "agent-1")
		}()
	}
	waitFor(t, "the open attempt to begin", func() bool { return blocking.attempts() == 1 })
	close(release)
	wait.Wait()

	for index, err := range results {
		if err != nil {
			t.Fatalf("viewer %d failed to subscribe: %v", index, err)
		}
	}
	if got := sessions.openCount(); got != 1 {
		t.Fatalf("opened %d streams for %d concurrent viewers, want 1", got, viewers)
	}
}

type blockingSessions struct {
	inner   *fakeSessions
	release chan struct{}

	mu    sync.Mutex
	tries int
}

func (s *blockingSessions) Open(ctx context.Context, agentID string) (FrameStream, error) {
	s.mu.Lock()
	s.tries++
	s.mu.Unlock()
	<-s.release
	return s.inner.Open(ctx, agentID)
}

func (s *blockingSessions) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries
}

// A slow browser misses whole rounds and is told how many. It must never make
// the Server queue frames on its behalf, and it must never slow down a viewer
// that is keeping up.
func TestSlowViewerCoalescesFramesAndCountsWhatItMissed(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	ctx := context.Background()

	slow, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("subscribe slow viewer: %v", err)
	}
	quick, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("subscribe quick viewer: %v", err)
	}

	stream := sessions.current()
	for round := 1; round <= 4; round++ {
		stream.push(sampleFrame("a"))
		// The quick viewer reads every round, which also proves the slow one is
		// not holding the relay up.
		if view := nextView(t, quick); view.ViewerDropped != 0 {
			t.Fatalf("the quick viewer was told it dropped %d rounds", view.ViewerDropped)
		}
	}
	waitFor(t, "the slow viewer to fall behind", func() bool { return slow.DroppedViews() == 3 })

	view := nextView(t, slow)
	if view.ViewerDropped != 3 {
		t.Fatalf("the slow viewer's view reported %d dropped rounds, want 3", view.ViewerDropped)
	}
	if len(view.Containers) != 1 || view.Containers[0].ContainerID != "a" {
		t.Fatalf("the coalesced view lost content: %+v", view.Containers)
	}
}

// The Agent's own drop count and the Server's are different failures and stay
// separate all the way to the viewer.
func TestAgentDropsAndViewerDropsAreReportedApart(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := sampleFrame("a")
	frame.DroppedFrames = 7
	sessions.current().push(frame)

	view := nextView(t, viewer)
	if view.AgentDropped != 7 || view.ViewerDropped != 0 {
		t.Fatalf("view reported agent=%d viewer=%d, want agent=7 viewer=0", view.AgentDropped, view.ViewerDropped)
	}
}

// A pending member is a row, not an omission. A container that exists and has
// not reported yet must not look like one that is gone.
func TestPendingMembersAppearAsRows(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := sampleFrame("b")
	frame.PendingContainerIDs = []string{"a"}
	sessions.current().push(frame)

	view := nextView(t, viewer)
	if len(view.Containers) != 2 {
		t.Fatalf("view carried %d rows, want the sampled and the pending one", len(view.Containers))
	}
	if view.Containers[0].ContainerID != "a" || !view.Containers[0].Pending {
		t.Fatalf("row 0 is %+v, want pending container a first", view.Containers[0])
	}
	if view.Containers[1].Pending {
		t.Fatalf("the sampled container was reported as pending")
	}
}

// The host row and both staleness reasons travel unchanged. The Server does
// not re-derive them: the Agent is the only place that knows why a refresh
// failed.
func TestHostRowAndStalenessTravelUnchanged(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := sampleFrame("a")
	frame.Workload.Filesystems = []producttransport.ManagedFilesystem{
		{Path: "/srv", TotalBytes: 100, FreeBytes: 40},
		{Path: "/gone", Unavailable: true, Reason: "no such directory"},
	}
	frame.MembershipStale, frame.MembershipReason = true, "docker listing failed"
	frame.WorkloadStale, frame.WorkloadReason = true, "engine info failed"
	sessions.current().push(frame)

	view := nextView(t, viewer)
	if view.Host.CPUCapacity != 4 || view.Host.MemoryCapacity != 8<<30 || view.Host.ContainersTotal != 1 {
		t.Fatalf("host row is %+v", view.Host)
	}
	if len(view.Host.Filesystems) != 2 || view.Host.Filesystems[1].Reason != "no such directory" {
		t.Fatalf("managed filesystems are %+v", view.Host.Filesystems)
	}
	if !view.MembershipStale || view.MembershipReason != "docker listing failed" {
		t.Fatalf("membership staleness is %v %q", view.MembershipStale, view.MembershipReason)
	}
	if !view.WorkloadStale || view.WorkloadReason != "engine info failed" {
		t.Fatalf("workload staleness is %v %q", view.WorkloadStale, view.WorkloadReason)
	}
}

// A stream that fails reaches every viewer with the reason, and the next
// subscribe opens a fresh one rather than reattaching to the dead relay.
func TestStreamFailureReachesViewersAndAllowsReopening(t *testing.T) {
	sessions := &fakeSessions{}
	hub := newTestHub(t, sessions)
	ctx := context.Background()
	first, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("subscribe first: %v", err)
	}
	second, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("subscribe second: %v", err)
	}

	agentGone := errors.New("agent session closed")
	sessions.current().fail(agentGone)

	for name, viewer := range map[string]*Subscription{"first": first, "second": second} {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := viewer.Next(waitCtx)
		cancel()
		if !errors.Is(err, agentGone) {
			t.Fatalf("%s viewer ended with %v, want the transport failure", name, err)
		}
	}
	waitFor(t, "the failed stream to be closed", func() bool { return sessions.closeCount() == 1 })

	if _, err := hub.Subscribe(ctx, "agent-1"); err != nil {
		t.Fatalf("resubscribe after failure: %v", err)
	}
	if got := sessions.openCount(); got != 2 {
		t.Fatalf("opened %d streams, want a fresh one after the failure", got)
	}
}

// Closing the hub ends every relay and releases every viewer.
func TestCloseEndsEveryRelay(t *testing.T) {
	sessions := &fakeSessions{}
	hub, err := New(Config{Sessions: sessions})
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("close hub: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := viewer.Next(waitCtx); err == nil {
		t.Fatal("a viewer kept receiving after the hub closed")
	}
	waitFor(t, "the Agent stream to close", func() bool { return sessions.closeCount() == 1 })
	if _, err := hub.Subscribe(context.Background(), "agent-1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after close returned %v, want ErrClosed", err)
	}
}
