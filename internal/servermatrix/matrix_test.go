package servermatrix

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/producttransport"
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

// fakeContext is discovery as this package sees it: a mapping a test can
// change, an error it can inject, and a count of how often it was asked.
type fakeContext struct {
	mu      sync.Mutex
	mapping map[string]ContainerContext
	err     error
	calls   int
}

func (c *fakeContext) ContainerContext(context.Context, string) (map[string]ContainerContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	mapping := make(map[string]ContainerContext, len(c.mapping))
	for id, value := range c.mapping {
		mapping[id] = value
	}
	return mapping, nil
}

func (c *fakeContext) set(mapping map[string]ContainerContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mapping = mapping
}

func (c *fakeContext) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *fakeContext) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// testClock advances only when a test says so, so refresh intervals are
// decided by the test rather than by how long it happened to take.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

func newTestHub(t *testing.T, sessions Sessions) *Hub {
	t.Helper()
	hub, _, _ := newContextHub(t, sessions)
	return hub
}

func newContextHub(t *testing.T, sessions Sessions) (*Hub, *fakeContext, *testClock) {
	t.Helper()
	source := &fakeContext{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0).UTC()}
	hub, err := New(Config{
		Sessions: sessions, Context: source, Clock: clock,
		ContextRefresh: time.Minute, ContextRetryInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, source, clock
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

// flatContainers reads the tree back as one list in container-ID order, which
// is what the fan-out tests are asserting about; the grouping itself has its
// own tests.
func flatContainers(view View) []ContainerRow {
	var rows []ContainerRow
	for _, project := range view.Projects {
		for _, service := range project.Services {
			rows = append(rows, service.Containers...)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ContainerID < rows[j].ContainerID })
	return rows
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

// viewUntil drives frames through the relay until a view satisfies the
// condition. Context refreshes settle asynchronously, so what a test is really
// waiting for is the first view assembled after one landed.
func viewUntil(t *testing.T, viewer *Subscription, stream *fakeStream, frame producttransport.MetricsMatrixFrame, what string, condition func(View) bool) View {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.push(frame)
		view := nextView(t, viewer)
		if condition(view) {
			return view
		}
	}
	t.Fatalf("timed out waiting for %s", what)
	return View{}
}

// staysAt asserts a count has stopped moving, which is how a test says "and
// then nothing else happened" about work that runs on its own goroutine.
func staysAt(t *testing.T, what string, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := count(); got != want {
			t.Fatalf("%s reached %d, want it to stay at %d", what, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
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
		rows := flatContainers(view)
		if len(rows) != 2 || rows[0].ContainerID != "a" || rows[1].ContainerID != "b" {
			t.Fatalf("%s viewer saw %+v, want both containers in ID order", name, rows)
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
	if rows := flatContainers(nextView(t, second)); len(rows) != 3 {
		t.Fatalf("the remaining viewer stopped receiving after the other left: %+v", rows)
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
	if rows := flatContainers(view); len(rows) != 1 || rows[0].ContainerID != "a" {
		t.Fatalf("the coalesced view lost content: %+v", rows)
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

	rows := flatContainers(nextView(t, viewer))
	if len(rows) != 2 {
		t.Fatalf("view carried %d rows, want the sampled and the pending one", len(rows))
	}
	if rows[0].ContainerID != "a" || !rows[0].Pending {
		t.Fatalf("row 0 is %+v, want pending container a first", rows[0])
	}
	if rows[1].Pending {
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
	hub, err := New(Config{Sessions: sessions, Context: &fakeContext{}})
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

// Project, service and image arrive from discovery, and a container discovery
// does not know is still a row with its metrics on it. The Engine decides what
// is running; discovery only says what is known about it.
func TestUnmappedContainersKeepTheirMetrics(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, _ := newContextHub(t, sessions)
	source.set(map[string]ContainerContext{
		"a": {ProjectUID: "uid-1", ProjectName: "shop", Service: "web", Image: "nginx:1"},
	})

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := sampleFrame("a", "b")
	frame.Containers[1].CPUPercent = 42
	sessions.current().push(frame)

	view := nextView(t, viewer)
	rows := flatContainers(view)
	mapped, unmapped := rows[0], rows[1]
	if mapped.Unmapped || mapped.ProjectName != "shop" || mapped.Service != "web" || mapped.Image != "nginx:1" {
		t.Fatalf("mapped row is %+v, want the discovery context joined on", mapped)
	}
	if mapped.ProjectUID != "uid-1" {
		t.Fatalf("mapped row carries project UID %q", mapped.ProjectUID)
	}
	if !unmapped.Unmapped || unmapped.ProjectName != "" || unmapped.Service != "" {
		t.Fatalf("unmapped row is %+v, want unknown project and service", unmapped)
	}
	if unmapped.Sample.CPUPercent != 42 {
		t.Fatalf("the unmapped container lost its metrics: %+v", unmapped.Sample)
	}
	if view.ContextStale {
		t.Fatal("context was reported stale after a successful lookup")
	}
}

// Knowing a container's image while knowing no project for it is the ordinary
// state of a container somebody started by hand. It is unmapped, and it is
// still shown.
func TestAContainerKnownOnlyByItsImageIsStillUnmapped(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, _ := newContextHub(t, sessions)
	source.set(map[string]ContainerContext{"a": {Image: "redis:7"}})

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sessions.current().push(sampleFrame("a"))
	view := nextView(t, viewer)

	row := flatContainers(view)[0]
	if !row.Unmapped || row.Image != "redis:7" {
		t.Fatalf("row is %+v, want an unmapped container that still shows its image", row)
	}
	if len(view.Projects) != 1 || !view.Projects[0].Unmapped {
		t.Fatalf("projects are %+v, want the unmapped bucket alone", view.Projects)
	}
}

// Discovery failing is not the same as a container being unmanaged, and the
// view says which one it is. The previous mapping stays rather than every row
// losing its project name over one failed call.
func TestFailedContextLookupIsSaidRatherThanShown(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, clock := newContextHub(t, sessions)
	source.set(map[string]ContainerContext{"a": {ProjectName: "shop", Service: "web"}})

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sessions.current().push(sampleFrame("a"))
	if rows := flatContainers(nextView(t, viewer)); rows[0].ProjectName != "shop" {
		t.Fatalf("opening view lacked context: %+v", rows[0])
	}

	source.fail(errors.New("agent is offline"))
	before := source.callCount()
	clock.advance(2 * time.Minute)
	sessions.current().push(sampleFrame("a"))
	waitFor(t, "the periodic context refresh to fail", func() bool { return source.callCount() > before })

	view := viewUntil(t, viewer, sessions.current(), sampleFrame("a"), "the context failure to be reported",
		func(view View) bool { return view.ContextStale })
	if view.ContextReason != "agent is offline" {
		t.Fatalf("context reason is %q, want the lookup failure", view.ContextReason)
	}
	if rows := flatContainers(view); rows[0].ProjectName != "shop" || rows[0].Unmapped {
		t.Fatalf("a failed lookup erased the last known context: %+v", rows[0])
	}
}

// A container discovery has never heard of is asked about once. Asking every
// frame would turn one hand-started container into a discovery call every few
// seconds, forever, for an answer that will not change.
func TestAnUnknownContainerIsAskedAboutOnce(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, clock := newContextHub(t, sessions)

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	opening := source.callCount()
	if opening != 1 {
		t.Fatalf("subscribe made %d context lookups, want one before the first frame", opening)
	}

	// The first frame carries a container the opening lookup did not cover, so
	// one early refresh is due once the retry floor has passed.
	sessions.current().push(sampleFrame("a"))
	nextView(t, viewer)
	clock.advance(11 * time.Second)
	sessions.current().push(sampleFrame("a"))
	nextView(t, viewer)
	waitFor(t, "the early refresh for a new container", func() bool { return source.callCount() == 2 })

	// It is still unmapped, and now known to be. Further frames must not keep
	// asking, however much time passes short of the periodic interval.
	for round := 0; round < 3; round++ {
		clock.advance(11 * time.Second)
		sessions.current().push(sampleFrame("a"))
		nextView(t, viewer)
	}
	staysAt(t, "context lookups for a container discovery does not manage", source.callCount, 2)

	// A container that appears later is new, and is worth one ask of its own.
	source.set(map[string]ContainerContext{"b": {ProjectName: "shop", Service: "api"}})
	clock.advance(11 * time.Second)
	sessions.current().push(sampleFrame("a", "b"))
	nextView(t, viewer)
	waitFor(t, "the newly deployed container to be looked up", func() bool { return source.callCount() == 3 })

	view := viewUntil(t, viewer, sessions.current(), sampleFrame("a", "b"), "the new container to be mapped",
		func(view View) bool { return !flatContainers(view)[1].Unmapped })
	if rows := flatContainers(view); rows[1].Service != "api" {
		t.Fatalf("the new container did not pick up its context: %+v", rows[1])
	}
}

// Steady state costs nothing. Frames arrive every couple of seconds; discovery
// is asked on its own far slower cadence.
func TestSteadyStateDoesNotAskDiscoveryPerFrame(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, clock := newContextHub(t, sessions)
	source.set(map[string]ContainerContext{"a": {ProjectName: "shop", Service: "web"}})

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	for round := 0; round < 10; round++ {
		clock.advance(2 * time.Second)
		sessions.current().push(sampleFrame("a"))
		nextView(t, viewer)
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("discovery was asked %d times across ten frames, want once", got)
	}

	clock.advance(time.Minute)
	sessions.current().push(sampleFrame("a"))
	nextView(t, viewer)
	waitFor(t, "the periodic refresh", func() bool { return source.callCount() == 2 })
}
