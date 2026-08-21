package livematrix

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/livestats"
)

type fakeMembership struct {
	mu      sync.Mutex
	running []string
	calls   int
	err     error
}

func (m *fakeMembership) Running(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return append([]string(nil), m.running...), nil
}

func (m *fakeMembership) set(ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = append([]string(nil), ids...)
}

func (m *fakeMembership) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type fakeEvents struct {
	mu      sync.Mutex
	notify  func()
	watches int
	ended   int
}

func (e *fakeEvents) Watch(ctx context.Context, changed func()) error {
	e.mu.Lock()
	e.notify = changed
	e.watches++
	e.mu.Unlock()
	<-ctx.Done()
	e.mu.Lock()
	e.ended++
	e.mu.Unlock()
	return ctx.Err()
}

func (e *fakeEvents) fire() {
	e.mu.Lock()
	notify := e.notify
	e.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func (e *fakeEvents) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.watches, e.ended
}

type fakeWorkload struct {
	mu       sync.Mutex
	capacity Capacity
	calls    int
	err      error
}

func (w *fakeWorkload) Capacity(context.Context) (Capacity, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.err != nil {
		return Capacity{}, w.err
	}
	return w.capacity, nil
}

func (w *fakeWorkload) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *fakeWorkload) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// manualTicker lets a test decide exactly when a frame is assembled.
type manualTicker struct{ c chan time.Time }

func (t *manualTicker) C() <-chan time.Time { return t.c }
func (t *manualTicker) Stop()               {}

type manualTickerFactory struct {
	mu      sync.Mutex
	tickers []*manualTicker
}

func (f *manualTickerFactory) NewTicker(time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	ticker := &manualTicker{c: make(chan time.Time, 1)}
	f.tickers = append(f.tickers, ticker)
	return ticker
}

func (f *manualTickerFactory) tick(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		count := len(f.tickers)
		var ticker *manualTicker
		if count > 0 {
			ticker = f.tickers[count-1]
		}
		f.mu.Unlock()
		if ticker != nil {
			ticker.c <- time.Now()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no ticker was created")
		}
		time.Sleep(time.Millisecond)
	}
}

// statsSource feeds livestats. Containers named in slow never emit, which is
// how a pending row is produced. A container given an end channel by
// endFirstStream has its first stream fail, which is how a stats stream that
// stops while its container keeps running is produced.
type statsSource struct {
	mu       sync.Mutex
	slow     map[string]bool
	ends     map[string]chan struct{}
	attempts map[string]int
}

func (s *statsSource) Stream(ctx context.Context, containerID string, emit func(livestats.Sample) error) error {
	s.mu.Lock()
	slow := s.slow[containerID]
	if s.attempts == nil {
		s.attempts = make(map[string]int)
	}
	s.attempts[containerID]++
	// The end channel is consumed by the attempt that gets it, so a
	// resubscription is a healthy stream rather than an instant failure again.
	end := s.ends[containerID]
	delete(s.ends, containerID)
	s.mu.Unlock()
	if !slow {
		if err := emit(livestats.Sample{ContainerID: containerID, MemoryUsage: 1}); err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-end:
		// A nil channel blocks forever, so a container without an end
		// channel behaves exactly as it did before.
		return errors.New("stats stream ended")
	}
}

// endFirstStream arranges for the next stream of containerID to fail when the
// returned channel is closed, without the container leaving the running set.
func (s *statsSource) endFirstStream(containerID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ends == nil {
		s.ends = make(map[string]chan struct{})
	}
	end := make(chan struct{})
	s.ends[containerID] = end
	return end
}

func (s *statsSource) streamAttempts(containerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[containerID]
}

type harness struct {
	hub        *Hub
	membership *fakeMembership
	events     *fakeEvents
	workload   *fakeWorkload
	tickers    *manualTickerFactory
	statsHub   *livestats.Hub
}

func newHarness(t *testing.T, source *statsSource, running ...string) *harness {
	t.Helper()
	return newHarnessReconcilingEvery(t, source, 1000, running...)
}

// newHarnessReconcilingEvery exposes the reconcile cadence for the tests that
// are about it. The default is deliberately far away so that a test which ticks
// for a frame does not get a membership refresh it did not ask for.
func newHarnessReconcilingEvery(t *testing.T, source *statsSource, reconcileEvery int, running ...string) *harness {
	t.Helper()
	statsHub, err := livestats.New(livestats.Config{Source: source, SampleInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statsHub.Close() })

	membership := &fakeMembership{running: append([]string(nil), running...)}
	events := &fakeEvents{}
	workload := &fakeWorkload{capacity: Capacity{CPUCapacity: 4, MemoryCapacity: 8 << 30, ContainersTotal: 9}}
	tickers := &manualTickerFactory{}
	hub, err := New(Config{
		Stats: statsHub, Membership: membership, Events: events, Workload: workload,
		FrameInterval: time.Hour, ReconcileEvery: reconcileEvery, TickerFactory: tickers,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return &harness{hub: hub, membership: membership, events: events, workload: workload, tickers: tickers, statsHub: statsHub}
}

func nextFrame(t *testing.T, subscription *Subscription) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frame, err := subscription.Next(ctx)
	if err != nil {
		t.Fatalf("waiting for a frame: %v", err)
	}
	return frame
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestAFrameAgreesWithItself is the acceptance condition that is easiest to
// lose: membership is snapshotted once, and the row list and the running count
// are computed from that one snapshot. A container leaving mid-assembly belongs
// in the next frame, never half of this one.
func TestAFrameAgreesWithItself(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a", "b", "c")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	waitFor(t, "initial reconcile", func() bool { return h.membership.callCount() >= 1 })
	h.tickers.tick(t)
	frame := nextFrame(t, viewer)

	if int(frame.Running) != len(frame.Rows) {
		t.Fatalf("frame says %d running but lists %d rows", frame.Running, len(frame.Rows))
	}
	if len(frame.Rows) != 3 {
		t.Fatalf("rows = %+v, want three", frame.Rows)
	}
	if frame.Capacity.CPUCapacity != 4 || frame.Capacity.ContainersTotal != 9 {
		t.Fatalf("capacity = %+v", frame.Capacity)
	}
}

// TestASlowContainerDoesNotHoldUpTheFrame: one container that has not produced
// a sample leaves its own row pending and nothing else.
func TestASlowContainerDoesNotHoldUpTheFrame(t *testing.T) {
	source := &statsSource{slow: map[string]bool{"slow": true}}
	h := newHarness(t, source, "fast", "slow")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "membership", func() bool { return h.membership.callCount() >= 1 })

	// The fast container's first sample is asynchronous, so the property is
	// that it arrives - not that it has arrived by any particular tick. What
	// must hold at every tick is that the slow container never blocks the
	// frame: both rows are present each time, and the slow one stays pending.
	settled := false
	for attempt := 0; attempt < 50 && !settled; attempt++ {
		h.tickers.tick(t)
		frame := nextFrame(t, viewer)
		if len(frame.Rows) != 2 {
			t.Fatalf("a pending container removed a row: %+v", frame.Rows)
		}
		byID := map[string]Row{}
		for _, row := range frame.Rows {
			byID[row.ContainerID] = row
		}
		if !byID["slow"].Pending {
			t.Fatalf("a container that never emitted was reported as sampled: %+v", byID["slow"])
		}
		settled = !byID["fast"].Pending
		if !settled {
			time.Sleep(time.Millisecond)
		}
	}
	if !settled {
		t.Fatal("the fast container never left pending, so the frame was waiting on something")
	}
}

// TestMembershipFollowsDockerAndIsIdempotent covers both paths that change
// membership - an event and a reconcile - and requires that they cannot start a
// second relay for a container or close one twice.
func TestMembershipFollowsDockerAndIsIdempotent(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	// A container appears after the viewer subscribed. Without event-driven
	// membership it would never be in a frame.
	h.membership.set("a", "b")
	h.events.fire()
	waitFor(t, "event reconcile", func() bool { return h.membership.callCount() >= 2 })
	h.tickers.tick(t)
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 2 {
		t.Fatalf("a container added after subscribe is missing: %+v", frame.Rows)
	}

	// The same event again must change nothing.
	h.events.fire()
	h.events.fire()
	waitFor(t, "repeat reconciles", func() bool { return h.membership.callCount() >= 4 })
	h.tickers.tick(t)
	frame = nextFrame(t, viewer)
	if len(frame.Rows) != 2 || int(frame.Running) != 2 {
		t.Fatalf("repeated events changed membership: %+v", frame.Rows)
	}

	// A container leaves.
	h.membership.set("a")
	h.events.fire()
	waitFor(t, "removal reconcile", func() bool { return h.membership.callCount() >= 5 })
	h.tickers.tick(t)
	frame = nextFrame(t, viewer)
	if len(frame.Rows) != 1 || frame.Rows[0].ContainerID != "a" {
		t.Fatalf("a removed container is still in the frame: %+v", frame.Rows)
	}
}

// TestAFailedListingKeepsThePreviousMembership: one failed Docker call must
// degrade a frame, not empty the view.
func TestAFailedListingKeepsThePreviousMembership(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a", "b")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	h.membership.mu.Lock()
	h.membership.err = errors.New("engine unavailable")
	h.membership.mu.Unlock()
	h.events.fire()
	waitFor(t, "failed reconcile", func() bool { return h.membership.callCount() >= 2 })

	h.tickers.tick(t)
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 2 {
		t.Fatalf("a failed listing emptied the view: %+v", frame.Rows)
	}
	// Keeping the rows is only half of it. A frame that keeps them without
	// saying so asserts they are current, and they are not - membership is
	// unknown, which is a third state and has to be visible.
	if !frame.MembershipStale {
		t.Fatal("stale rows were presented as current")
	}
	if frame.MembershipReason == "" {
		t.Fatal("a stale frame gave no reason")
	}

	// Recovery clears it.
	h.membership.mu.Lock()
	h.membership.err = nil
	h.membership.mu.Unlock()
	before := h.membership.callCount()
	h.events.fire()
	waitFor(t, "recovery reconcile", func() bool { return h.membership.callCount() > before })
	h.tickers.tick(t)
	frame = nextFrame(t, viewer)
	if frame.MembershipStale || frame.MembershipReason != "" {
		t.Fatalf("a recovered frame is still marked stale: %+v", frame)
	}
}

// TestAnEngineThatNeverAnsweredIsNotAnEmptyHost is the first-contact case: no
// rows and no reason would read as "this host runs nothing", which is a claim
// about the host rather than about Docker.
func TestAnEngineThatNeverAnsweredIsNotAnEmptyHost(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source)
	h.membership.mu.Lock()
	h.membership.err = errors.New("Cannot connect to the Docker daemon")
	h.membership.mu.Unlock()

	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	h.tickers.tick(t)
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 0 || frame.Running != 0 {
		t.Fatalf("rows appeared from nowhere: %+v", frame.Rows)
	}
	if !frame.MembershipStale {
		t.Fatal("an unreachable Engine was reported as a host with no containers")
	}
}

// TestViewersShareOneRelayAndTheLastOneStopsIt is the lifecycle condition: the
// event watch, the ticker and every container subscription exist once and are
// all released when the last viewer leaves.
func TestViewersShareOneRelayAndTheLastOneStopsIt(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a", "b")
	first, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })
	second, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	watches, _ := h.events.counts()
	if watches != 1 {
		t.Fatalf("a second viewer started %d event watches, want the first one shared", watches)
	}
	h.tickers.mu.Lock()
	tickerCount := len(h.tickers.tickers)
	h.tickers.mu.Unlock()
	if tickerCount != 1 {
		t.Fatalf("ticker count = %d, want one shared", tickerCount)
	}

	// Both viewers see the same frame.
	h.tickers.tick(t)
	if a, b := nextFrame(t, first), nextFrame(t, second); len(a.Rows) != len(b.Rows) {
		t.Fatalf("viewers disagree: %d vs %d rows", len(a.Rows), len(b.Rows))
	}

	// One leaving does not stop collection.
	_ = first.Close()
	h.tickers.tick(t)
	_ = nextFrame(t, second)

	// The last one does.
	_ = second.Close()
	waitFor(t, "event watch to end", func() bool { _, ended := h.events.counts(); return ended == 1 })
	h.hub.mu.Lock()
	relay := h.hub.relay
	h.hub.mu.Unlock()
	if relay != nil {
		t.Fatal("the relay outlived its last viewer")
	}
}

// TestASlowViewerLosesWholeFramesAndIsToldHowMany pins the backpressure rule: a
// viewer holds one frame, and what it missed is counted rather than queued.
func TestASlowViewerLosesWholeFramesAndIsToldHowMany(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	for range 5 {
		h.tickers.tick(t)
		waitFor(t, "frame to land", func() bool {
			viewer.mu.Lock()
			defer viewer.mu.Unlock()
			return viewer.has
		})
		// Deliberately not reading, so the next frame overwrites this one.
		viewer.mu.Lock()
		viewer.has = true
		viewer.mu.Unlock()
	}
	if dropped := viewer.DroppedFrames(); dropped == 0 {
		t.Fatal("a viewer that never read reported no dropped frames")
	}
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 1 {
		t.Fatalf("the surviving frame is incomplete: %+v", frame.Rows)
	}
}

// TestPendingMeansOnlyAwaitingItsFirstSample pins what pending is and is not.
// It is membership minus what has reported - never a way to describe a failed
// listing, and never overlapping the rows that did report.
func TestPendingMeansOnlyAwaitingItsFirstSample(t *testing.T) {
	source := &statsSource{slow: map[string]bool{"quiet": true}}
	h := newHarness(t, source, "loud", "quiet")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	for attempt := 0; attempt < 50; attempt++ {
		h.tickers.tick(t)
		frame := nextFrame(t, viewer)

		members := map[string]bool{}
		pending := map[string]int{}
		sampled := map[string]bool{}
		for _, row := range frame.Rows {
			members[row.ContainerID] = true
			if row.Pending {
				pending[row.ContainerID]++
			} else {
				sampled[row.ContainerID] = true
			}
		}
		for id, count := range pending {
			if count != 1 {
				t.Fatalf("%q appears pending %d times", id, count)
			}
			if !members[id] {
				t.Fatalf("%q is pending but not a member", id)
			}
			if sampled[id] {
				t.Fatalf("%q is both pending and sampled", id)
			}
		}
		if _, quietPending := pending["quiet"]; !quietPending {
			t.Fatalf("a container that never emitted is not pending: %+v", frame.Rows)
		}
		if sampled["loud"] {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the emitting container never produced a sample")
}

// TestMembershipAndWorkloadFailIndependently: the container listing and the
// Engine's own capacity are different calls, and one failing must not describe
// the other as unknown.
func TestMembershipAndWorkloadFailIndependently(t *testing.T) {
	source := &statsSource{}
	h := newHarnessReconcilingEvery(t, source, 1, "a", "b")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })

	// Engine info fails; the container listing does not.
	h.workload.fail(errors.New("engine info unavailable"))
	h.tickers.tick(t)
	frame := nextFrame(t, viewer)
	if frame.MembershipStale {
		t.Fatalf("an Engine info failure marked the container rows stale: %+v", frame)
	}
	if !frame.WorkloadStale || frame.WorkloadReason == "" {
		t.Fatalf("an Engine info failure was not reported: %+v", frame)
	}
	if len(frame.Rows) != 2 {
		t.Fatalf("rows were lost to an unrelated failure: %+v", frame.Rows)
	}
	if frame.Capacity.CPUCapacity != 4 {
		t.Fatalf("a failed refresh discarded the last known capacity: %+v", frame.Capacity)
	}

	// The reverse: the listing fails while Engine info recovers. The workload
	// half must refresh even though the membership half could not, because a
	// Docker listing failure says nothing about filesystem capacity.
	h.workload.fail(nil)
	h.membership.mu.Lock()
	h.membership.err = errors.New("listing unavailable")
	h.membership.mu.Unlock()
	h.tickers.tick(t)
	frame = nextFrame(t, viewer)
	if !frame.MembershipStale || frame.MembershipReason == "" {
		t.Fatalf("a failed listing was not reported: %+v", frame)
	}
	if frame.WorkloadStale {
		t.Fatalf("a failed listing kept the workload summary from refreshing: %+v", frame)
	}
}

// TestAnEventBurstDoesNotReQueryTheEngine separates the two cadences. Container
// lifecycle events say membership moved; they say nothing about how many CPUs
// the host has, and turning each one into an Engine info call would make a
// noisy host pay for a number that changes when the machine reboots.
func TestAnEventBurstDoesNotReQueryTheEngine(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return h.membership.callCount() >= 1 })
	afterStart := h.workload.callCount()

	for i := 0; i < 50; i++ {
		h.events.fire()
	}
	waitFor(t, "event-driven reconciles", func() bool { return h.membership.callCount() > 1 })
	// Give any stray Engine call time to land before concluding none happened.
	time.Sleep(20 * time.Millisecond)
	if got := h.workload.callCount(); got != afterStart {
		t.Fatalf("an event burst re-queried the Engine %d times", got-afterStart)
	}
}

// TestAnEventBurstCoalescesIntoSequentialReconciles is the bound that keeps a
// noisy host from turning into a reconcile storm. A hundred events must not
// become a hundred concurrent listings: the signal is collapsed and the
// reconciles run one after another on the relay's own goroutine.
func TestAnEventBurstCoalescesIntoSequentialReconciles(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")

	// Count how many listings overlap. Sequential reconciles never exceed one.
	var concurrent, peak int
	var peakMu sync.Mutex
	h.membership.mu.Lock()
	h.membership.mu.Unlock()
	original := h.membership
	gate := &gatedMembership{inner: original, onEnter: func() {
		peakMu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		peakMu.Unlock()
		time.Sleep(time.Millisecond)
	}, onExit: func() {
		peakMu.Lock()
		concurrent--
		peakMu.Unlock()
	}}
	hub, err := New(Config{
		Stats: h.statsHub, Membership: gate, Events: h.events, Workload: h.workload,
		FrameInterval: time.Hour, ReconcileEvery: 1000, TickerFactory: h.tickers,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	viewer, err := hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "first reconcile", func() bool { return original.callCount() >= 1 })

	for range 100 {
		h.events.fire()
	}
	// Let the relay work through whatever the burst produced.
	time.Sleep(200 * time.Millisecond)

	peakMu.Lock()
	observed := peak
	peakMu.Unlock()
	if observed > 1 {
		t.Fatalf("%d listings ran concurrently; an event burst must coalesce", observed)
	}
	if calls := original.callCount(); calls > 20 {
		t.Fatalf("100 events produced %d listings; the signal did not coalesce", calls)
	}
}

type gatedMembership struct {
	inner   *fakeMembership
	onEnter func()
	onExit  func()
}

func (g *gatedMembership) Running(ctx context.Context) ([]string, error) {
	g.onEnter()
	defer g.onExit()
	return g.inner.Running(ctx)
}

// TestAMemberIsResubscribedWhenItsStatsStreamEnds: a Docker stats stream can
// end while its container keeps running - a transient socket or decoder
// failure, or a restart quick enough that the same ID is listed again. The
// subscription is finished but still mapped, and a reconcile that only asks
// whether an ID is present would call the row watched and go on repeating the
// last sample for as long as the container lives. Reconcile has to notice the
// finished subscription and replace it.
func TestAMemberIsResubscribedWhenItsStatsStreamEnds(t *testing.T) {
	source := &statsSource{}
	end := source.endFirstStream("a")
	h := newHarnessReconcilingEvery(t, source, 1, "a")

	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	waitFor(t, "the first stats stream", func() bool { return source.streamAttempts("a") == 1 })
	h.tickers.tick(t)
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 1 {
		t.Fatalf("rows = %+v, want the one running container", frame.Rows)
	}

	// The stats stream fails. The container itself is untouched and stays in
	// the running set, so this is not a membership change.
	close(end)
	waitFor(t, "the ended stats stream to be dropped", func() bool { return h.statsHub.ActiveStreams() == 0 })

	h.tickers.tick(t)
	waitFor(t, "the member to be resubscribed", func() bool { return source.streamAttempts("a") >= 2 })

	// The replacement is a live stream, not a second dead entry.
	waitFor(t, "the replacement stream to be active", func() bool { return h.statsHub.ActiveStreams() == 1 })

	h.tickers.tick(t)
	frame = nextFrame(t, viewer)
	if len(frame.Rows) != 1 {
		t.Fatalf("rows = %+v, want the one running container", frame.Rows)
	}
	if frame.MembershipStale {
		t.Fatal("a stats stream ending is not a membership refresh failure")
	}
}
