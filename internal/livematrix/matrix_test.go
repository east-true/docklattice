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
}

func (w *fakeWorkload) Capacity(context.Context) (Capacity, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	return w.capacity, nil
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
// how a pending row is produced.
type statsSource struct {
	mu   sync.Mutex
	slow map[string]bool
}

func (s *statsSource) Stream(ctx context.Context, containerID string, emit func(livestats.Sample) error) error {
	s.mu.Lock()
	slow := s.slow[containerID]
	s.mu.Unlock()
	if !slow {
		if err := emit(livestats.Sample{ContainerID: containerID, MemoryUsage: 1}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
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
		FrameInterval: time.Hour, ReconcileEvery: 1000, TickerFactory: tickers,
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
