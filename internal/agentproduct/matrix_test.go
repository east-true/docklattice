package agentproduct

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/producttransport"
)

func containerID(letter string) string { return strings.Repeat(letter, 64) }

// fakeMatrixDocker is the Docker Engine as live metrics see it: a listing, a
// self-description, and an event stream, each of which can fail on its own.
type fakeMatrixDocker struct {
	mu sync.Mutex

	running   []string
	listErr   error
	listCalls int
	// listConcurrency records the deepest overlap of listing calls. Reconciles
	// run on the relay's own goroutine, so it must never exceed one.
	listing          int
	listConcurrency  int
	info             dockeradapter.EngineInfo
	infoErr          error
	infoCalls        int
	eventSubscribes  int
	eventChannels    []chan dockeradapter.Event
	openEventStreams int
}

func (d *fakeMatrixDocker) ListRunning(context.Context) ([]dockeradapter.Container, error) {
	d.mu.Lock()
	d.listCalls++
	d.listing++
	if d.listing > d.listConcurrency {
		d.listConcurrency = d.listing
	}
	d.mu.Unlock()
	d.mu.Lock()
	defer func() {
		d.listing--
		d.mu.Unlock()
	}()
	if d.listErr != nil {
		return nil, d.listErr
	}
	containers := make([]dockeradapter.Container, 0, len(d.running))
	for _, id := range d.running {
		containers = append(containers, dockeradapter.Container{ID: id, State: "running"})
	}
	return containers, nil
}

func (d *fakeMatrixDocker) Info(context.Context) (dockeradapter.EngineInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.infoCalls++
	if d.infoErr != nil {
		return dockeradapter.EngineInfo{}, d.infoErr
	}
	return d.info, nil
}

func (d *fakeMatrixDocker) SubscribeEvents(ctx context.Context, _ time.Time) (dockeradapter.EventStream, error) {
	events := make(chan dockeradapter.Event)
	errorsOut := make(chan error, 1)
	d.mu.Lock()
	d.eventSubscribes++
	d.openEventStreams++
	d.eventChannels = append(d.eventChannels, events)
	d.mu.Unlock()
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		d.openEventStreams--
		d.mu.Unlock()
		close(errorsOut)
	}()
	return dockeradapter.EventStream{Events: events, Errors: errorsOut}, nil
}

func (d *fakeMatrixDocker) setRunning(ids ...string) {
	d.mu.Lock()
	d.running = append([]string(nil), ids...)
	d.mu.Unlock()
}

func (d *fakeMatrixDocker) failListing(err error) {
	d.mu.Lock()
	d.listErr = err
	d.mu.Unlock()
}

func (d *fakeMatrixDocker) failInfo(err error) {
	d.mu.Lock()
	d.infoErr = err
	d.mu.Unlock()
}

// endEventStreams closes every subscription this fake has handed out, which is
// what a dropped Docker connection looks like to the watcher. Channels are
// taken under the lock and cleared, so each is closed exactly once.
func (d *fakeMatrixDocker) endEventStreams() int {
	d.mu.Lock()
	channels := d.eventChannels
	d.eventChannels = nil
	subscribes := d.eventSubscribes
	d.mu.Unlock()
	for _, channel := range channels {
		close(channel)
	}
	return subscribes
}

func (d *fakeMatrixDocker) counts() (listCalls, subscribes, openStreams, peakConcurrency int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listCalls, d.eventSubscribes, d.openEventStreams, d.listConcurrency
}

// fire delivers one container event to whichever subscription is listening.
func (d *fakeMatrixDocker) fire(t *testing.T, resourceType string) {
	t.Helper()
	d.mu.Lock()
	channels := append([]chan dockeradapter.Event(nil), d.eventChannels...)
	d.mu.Unlock()
	event := dockeradapter.Event{ResourceType: resourceType, Action: "start"}
	for _, channel := range channels {
		select {
		case channel <- event:
			return
		default:
		}
	}
}

// countingStatsSource records every container stream the matrix opens and
// closes, which is how "reused" is told apart from "restarted".
type countingStatsSource struct {
	mu     sync.Mutex
	opens  map[string]int
	closes map[string]int
}

func newCountingStatsSource() *countingStatsSource {
	return &countingStatsSource{opens: map[string]int{}, closes: map[string]int{}}
}

func (s *countingStatsSource) Stream(ctx context.Context, id string, emit func(livestats.Sample) error) error {
	s.mu.Lock()
	s.opens[id]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.closes[id]++
		s.mu.Unlock()
	}()
	if err := emit(livestats.Sample{ContainerID: id, MemoryUsage: 1}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *countingStatsSource) count(id string) (opens, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens[id], s.closes[id]
}

// matrixSender is one browser's worth of frames.
type matrixSender struct {
	mu     sync.Mutex
	frames []producttransport.MetricsMatrixFrame
}

func (s *matrixSender) Send(frame producttransport.MetricsMatrixFrame) error {
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	s.mu.Unlock()
	return nil
}

func (s *matrixSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *matrixSender) latest() (producttransport.MetricsMatrixFrame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return producttransport.MetricsMatrixFrame{}, false
	}
	return s.frames[len(s.frames)-1], true
}

type matrixViewer struct {
	sender *matrixSender
	cancel context.CancelFunc
	done   chan error
}

func (v *matrixViewer) stop(t *testing.T) {
	t.Helper()
	v.cancel()
	select {
	case <-v.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the metrics stream did not end when its viewer left")
	}
}

type matrixHarness struct {
	handler *Handler
	docker  *fakeMatrixDocker
	stats   *countingStatsSource
}

func newMatrixHarness(t *testing.T, running ...string) *matrixHarness {
	t.Helper()
	config, _, _ := validConfig(t)
	stats := newCountingStatsSource()
	docker := &fakeMatrixDocker{
		running: append([]string(nil), running...),
		info:    dockeradapter.EngineInfo{CPUCapacity: 6, MemoryCapacity: 32 << 30, ContainersTotal: 9},
	}
	config.StatsSource = stats
	config.MatrixDocker = docker
	config.MatrixFrameInterval = 5 * time.Millisecond
	config.matrixEventRetry = time.Millisecond
	handler, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	return &matrixHarness{handler: handler, docker: docker, stats: stats}
}

func (h *matrixHarness) watch(t *testing.T) *matrixViewer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	viewer := &matrixViewer{sender: &matrixSender{}, cancel: cancel, done: make(chan error, 1)}
	go func() {
		viewer.done <- h.handler.StreamMetricsMatrix(ctx, producttransport.SessionInfo{}, producttransport.MetricsMatrixRequest{}, viewer.sender)
	}()
	return viewer
}

func waitForMatrix(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForFrame waits for a frame that satisfies want. Frames are latest-wins,
// so a test asserts on the first frame that can carry the change rather than on
// a particular frame number.
func waitForFrame(t *testing.T, viewer *matrixViewer, what string, want func(producttransport.MetricsMatrixFrame) bool) producttransport.MetricsMatrixFrame {
	t.Helper()
	var last producttransport.MetricsMatrixFrame
	waitForMatrix(t, what, func() bool {
		frame, ok := viewer.sender.latest()
		if !ok {
			return false
		}
		last = frame
		return want(frame)
	})
	return last
}

func frameIDs(frame producttransport.MetricsMatrixFrame) map[string]bool {
	ids := make(map[string]bool, len(frame.Containers)+len(frame.PendingContainerIDs))
	for _, sample := range frame.Containers {
		ids[sample.ContainerID] = true
	}
	for _, id := range frame.PendingContainerIDs {
		ids[id] = true
	}
	return ids
}

// TestTheAgentServesAMatrixThroughItsWholeLifecycle is the Agent-side closure
// gate. It drives the production path - handler, transport bridge, livematrix,
// and the Docker adapters - through membership churn, each source failing on
// its own, and teardown, and asserts the meaning that was fixed for each.
func TestTheAgentServesAMatrixThroughItsWholeLifecycle(t *testing.T) {
	a, b, c, d := containerID("a"), containerID("b"), containerID("c"), containerID("d")
	h := newMatrixHarness(t, a, b, c)
	beforeGoroutines := runtime.NumGoroutine()

	viewer := h.watch(t)
	frame := waitForFrame(t, viewer, "the opening frame", func(frame producttransport.MetricsMatrixFrame) bool {
		return len(frameIDs(frame)) == 3
	})
	if ids := frameIDs(frame); !ids[a] || !ids[b] || !ids[c] {
		t.Fatalf("the opening frame is not the host: %+v", ids)
	}
	if frame.Workload.CPUCapacity != 6 || frame.Workload.ContainersTotal != 9 {
		t.Fatalf("the workload summary is not the Engine's: %+v", frame.Workload)
	}
	if frame.Workload.ContainersRunning != 3 {
		t.Fatalf("running count disagrees with the rows: %+v", frame.Workload)
	}

	// A leaves and D arrives. The two that stayed keep the stream they had.
	h.docker.setRunning(b, c, d)
	h.docker.fire(t, "container")
	frame = waitForFrame(t, viewer, "membership to follow Docker", func(frame producttransport.MetricsMatrixFrame) bool {
		ids := frameIDs(frame)
		return ids[d] && !ids[a]
	})
	if ids := frameIDs(frame); !ids[b] || !ids[c] {
		t.Fatalf("containers that stayed were lost: %+v", ids)
	}
	if opens, closes := h.stats.count(a); opens != 1 || closes != 1 {
		t.Fatalf("A's stream opened %d times and closed %d", opens, closes)
	}
	if opens, closes := h.stats.count(d); opens != 1 || closes != 0 {
		t.Fatalf("D's stream opened %d times and closed %d", opens, closes)
	}
	for name, id := range map[string]string{"B": b, "C": c} {
		if opens, closes := h.stats.count(id); opens != 1 || closes != 0 {
			t.Fatalf("%s's stream was restarted: opened %d, closed %d", name, opens, closes)
		}
	}

	// A failed listing is not an empty host: the rows stay and say so.
	h.docker.failListing(errors.New("engine listing unavailable"))
	h.docker.fire(t, "container")
	frame = waitForFrame(t, viewer, "a stale membership to be reported", func(frame producttransport.MetricsMatrixFrame) bool {
		return frame.MembershipStale
	})
	if frame.MembershipReason == "" {
		t.Fatal("a stale membership was reported without a reason")
	}
	if ids := frameIDs(frame); len(ids) != 3 || !ids[b] || !ids[c] || !ids[d] {
		t.Fatalf("a failed listing dropped the last known rows: %+v", ids)
	}
	if frame.WorkloadStale {
		t.Fatalf("a failed listing was reported as a workload failure too: %+v", frame)
	}

	h.docker.failListing(nil)
	waitForFrame(t, viewer, "membership to recover", func(frame producttransport.MetricsMatrixFrame) bool {
		return !frame.MembershipStale && len(frameIDs(frame)) == 3
	})

	// The Engine failing to describe itself is a different failure, and only
	// the workload half of the frame carries it.
	h.docker.failInfo(errors.New("engine info unavailable"))
	frame = waitForFrame(t, viewer, "a stale workload to be reported", func(frame producttransport.MetricsMatrixFrame) bool {
		return frame.WorkloadStale
	})
	if frame.MembershipStale {
		t.Fatalf("an Engine info failure marked the rows stale: %+v", frame)
	}
	if len(frameIDs(frame)) != 3 {
		t.Fatalf("an Engine info failure dropped rows: %+v", frameIDs(frame))
	}
	if frame.Workload.CPUCapacity != 6 {
		t.Fatalf("a failed refresh discarded the last known capacity: %+v", frame.Workload)
	}
	if frame.WorkloadReason == "" {
		t.Fatal("a stale workload was reported without a reason")
	}
	h.docker.failInfo(nil)
	waitForFrame(t, viewer, "the workload summary to recover", func(frame producttransport.MetricsMatrixFrame) bool {
		return !frame.WorkloadStale
	})

	// An event burst is one signal, not a hundred. Reconciles run in sequence
	// on the relay's goroutine, so listings never overlap.
	listsBefore, _, _, _ := h.docker.counts()
	for i := 0; i < 100; i++ {
		h.docker.fire(t, "container")
	}
	waitForMatrix(t, "the burst to be reconciled", func() bool {
		lists, _, _, _ := h.docker.counts()
		return lists > listsBefore
	})
	if _, _, _, peak := h.docker.counts(); peak > 1 {
		t.Fatalf("%d listings ran at once", peak)
	}

	// The last viewer leaving takes everything it started with it.
	viewer.stop(t)
	waitForMatrix(t, "the event subscription to close", func() bool {
		_, _, open, _ := h.docker.counts()
		return open == 0
	})
	waitForMatrix(t, "container streams to be released", func() bool {
		for _, id := range []string{b, c, d} {
			if opens, closes := h.stats.count(id); opens != closes {
				return false
			}
		}
		return true
	})
	listsAfterStop, _, _, _ := h.docker.counts()
	time.Sleep(50 * time.Millisecond)
	if lists, _, _, _ := h.docker.counts(); lists != listsAfterStop {
		t.Fatalf("the reconcile ticker kept running after the last viewer left: %d more listings", lists-listsAfterStop)
	}
	waitForMatrix(t, "goroutines to settle", func() bool {
		return runtime.NumGoroutine() <= beforeGoroutines+2
	})
}

// TestASecondViewerSharesTheFirstOnesCollection is the fan-out property at the
// Agent: two browsers watching one host cost one event subscription and one
// stream per container, and the first one leaving takes nothing away from the
// second.
func TestASecondViewerSharesTheFirstOnesCollection(t *testing.T) {
	a := containerID("a")
	h := newMatrixHarness(t, a)

	first := h.watch(t)
	waitForFrame(t, first, "the first viewer's frame", func(frame producttransport.MetricsMatrixFrame) bool {
		return len(frameIDs(frame)) == 1
	})
	_, subscribesAfterFirst, _, _ := h.docker.counts()

	second := h.watch(t)
	waitForFrame(t, second, "the second viewer's frame", func(frame producttransport.MetricsMatrixFrame) bool {
		return len(frameIDs(frame)) == 1
	})
	if _, subscribes, _, _ := h.docker.counts(); subscribes != subscribesAfterFirst {
		t.Fatalf("a second viewer opened its own event subscription: %d then %d", subscribesAfterFirst, subscribes)
	}
	if opens, _ := h.stats.count(a); opens != 1 {
		t.Fatalf("a second viewer opened its own container stream: %d", opens)
	}

	first.stop(t)
	framesAtStop := second.sender.count()
	waitForMatrix(t, "the remaining viewer to keep receiving", func() bool {
		return second.sender.count() > framesAtStop
	})
	if _, closes := h.stats.count(a); closes != 0 {
		t.Fatal("one viewer leaving stopped collection the other still needs")
	}
	second.stop(t)
	waitForMatrix(t, "collection to stop with the last viewer", func() bool {
		_, closes := h.stats.count(a)
		return closes == 1
	})
}

// TestOnlyContainerEventsCostAListing keeps image, volume and network activity
// off the membership path. They arrive on the same subscription and mean
// nothing about which containers are running.
func TestOnlyContainerEventsCostAListing(t *testing.T) {
	h := newMatrixHarness(t, containerID("a"))
	viewer := h.watch(t)
	defer viewer.stop(t)
	waitForFrame(t, viewer, "the opening frame", func(frame producttransport.MetricsMatrixFrame) bool {
		return len(frameIDs(frame)) == 1
	})
	waitForMatrix(t, "the event subscription", func() bool {
		_, _, open, _ := h.docker.counts()
		return open > 0
	})

	before, _, _, _ := h.docker.counts()
	for _, resourceType := range []string{"image", "volume", "network"} {
		h.docker.fire(t, resourceType)
	}
	time.Sleep(20 * time.Millisecond)
	if lists, _, _, _ := h.docker.counts(); lists != before {
		t.Fatalf("unrelated events cost %d listings", lists-before)
	}

	h.docker.fire(t, "container")
	waitForMatrix(t, "a container event to be reconciled", func() bool {
		lists, _, _, _ := h.docker.counts()
		return lists > before
	})
}

// TestOneUnreadablePathDoesNotHideTheOthers: managed filesystem capacity is
// per path. A discovery root that has gone is a fact about that root, and the
// remaining ones still report.
func TestOneUnreadablePathDoesNotHideTheOthers(t *testing.T) {
	workload := dockerWorkload{
		docker: &fakeMatrixDocker{info: dockeradapter.EngineInfo{CPUCapacity: 2}},
		paths:  []string{"/srv/projects", "/srv/other", "/var/lib/dockpilot"},
		probe: func(path string) (filesystemUsage, error) {
			switch path {
			case "/srv/other":
				return filesystemUsage{}, errors.New("no such file or directory")
			case "/var/lib/dockpilot":
				// Same device as /srv/projects: one filesystem, one row.
				return filesystemUsage{Device: 64, TotalBytes: 100, FreeBytes: 40}, nil
			default:
				return filesystemUsage{Device: 64, TotalBytes: 100, FreeBytes: 40}, nil
			}
		},
	}
	capacity, err := workload.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capacity.Filesystems) != 2 {
		t.Fatalf("expected one row per filesystem plus the unreadable path: %+v", capacity.Filesystems)
	}
	if capacity.Filesystems[0].Path != "/srv/projects" || capacity.Filesystems[0].TotalBytes != 100 {
		t.Fatalf("a readable path was not reported: %+v", capacity.Filesystems[0])
	}
	unreadable := capacity.Filesystems[1]
	if unreadable.Path != "/srv/other" || !unreadable.Unavailable || unreadable.Reason == "" {
		t.Fatalf("an unreadable path was not reported as such: %+v", unreadable)
	}
	if unreadable.TotalBytes != 0 || unreadable.FreeBytes != 0 {
		t.Fatalf("an unreadable path reported capacity: %+v", unreadable)
	}
}

// TestManagedFilesystemsAreNotAMountInventory pins the boundary. The host row
// answers how much room Dockpilot has where it writes; it does not enumerate
// the host's mounts, and a path is reported only because it was configured.
func TestManagedFilesystemsAreNotAMountInventory(t *testing.T) {
	var probed []string
	workload := dockerWorkload{
		docker: &fakeMatrixDocker{},
		paths:  []string{"/srv/projects", "", "/srv/projects"},
		probe: func(path string) (filesystemUsage, error) {
			probed = append(probed, path)
			return filesystemUsage{Device: 7, TotalBytes: 10, FreeBytes: 5}, nil
		},
	}
	capacity, err := workload.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capacity.Filesystems) != 1 {
		t.Fatalf("one filesystem reported more than once: %+v", capacity.Filesystems)
	}
	if len(probed) != 2 {
		t.Fatalf("paths probed = %v, want only the configured non-empty ones", probed)
	}
}

// TestAWorkloadFailureIsTheEnginesAlone: Capacity fails as a whole when the
// Engine cannot describe itself, because CPU and memory capacity have no last
// resort. Filesystem capacity is reported per path instead, above.
func TestAWorkloadFailureIsTheEnginesAlone(t *testing.T) {
	failure := errors.New("engine info unavailable")
	workload := dockerWorkload{
		docker: &fakeMatrixDocker{infoErr: failure},
		paths:  []string{"/srv/projects"},
		probe:  func(string) (filesystemUsage, error) { return filesystemUsage{Device: 1}, nil },
	}
	if _, err := workload.Capacity(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Capacity error = %v", err)
	}
}

// TestABrokenEventStreamResubscribesAndAsksAgain: events are an optimization,
// and losing them must not leave membership frozen until the next periodic
// repair. A new subscription starts by reconciling once, because it cannot know
// what happened while it was down.
func TestABrokenEventStreamResubscribesAndAsksAgain(t *testing.T) {
	docker := &fakeMatrixDocker{}
	events := dockerEvents{docker: docker, retry: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var changes int
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = events.Watch(ctx, func() {
			mu.Lock()
			changes++
			mu.Unlock()
		})
	}()

	// Break every subscription as soon as it appears. The watcher must keep
	// coming back rather than giving up on events for the relay's lifetime.
	waitForMatrix(t, "resubscription", func() bool {
		return docker.endEventStreams() >= 3
	})
	mu.Lock()
	observed := changes
	mu.Unlock()
	if observed < 3 {
		t.Fatalf("resubscribing did not ask for a fresh membership: %d reconciles", observed)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the event watch did not end with its context")
	}
}
