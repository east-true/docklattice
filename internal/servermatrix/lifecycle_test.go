package servermatrix

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// settledGoroutines gives the runtime a chance to reclaim what a test finished
// with, and reports the lowest count it saw. A leak shows up as a floor that
// never comes back down; ordinary scheduling noise does not.
func settledGoroutines(t *testing.T, ceiling int) int {
	t.Helper()
	best := runtime.NumGoroutine()
	for range 100 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		if count := runtime.NumGoroutine(); count < best {
			best = count
		}
		if best <= ceiling {
			return best
		}
	}
	return best
}

func (h *Hub) relayCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.relays)
}

// Nothing is collected for a host nobody is watching. This is the property the
// whole design rests on: an idle Server costs an idle Agent nothing.
func TestNothingIsOpenedWithoutAViewer(t *testing.T) {
	sessions := &fakeSessions{}
	hub, _, _ := newContextHub(t, sessions)

	if got := sessions.openCount(); got != 0 {
		t.Fatalf("a hub with no viewers opened %d Agent streams", got)
	}
	if got := hub.relayCount(); got != 0 {
		t.Fatalf("a hub with no viewers holds %d relays", got)
	}

	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sessions.current().push(sampleFrame("a"))
	nextView(t, viewer)
	if err := viewer.Close(); err != nil {
		t.Fatalf("close viewer: %v", err)
	}

	waitFor(t, "the relay to be released with its last viewer", func() bool { return hub.relayCount() == 0 })
	waitFor(t, "the Agent stream to close", func() bool { return sessions.closeCount() == 1 })
}

// Many viewers of one host are still one Agent stream, and the last one out
// closes it. The number of browsers watching must not be visible to the Agent.
func TestManyViewersAreStillOneAgentStream(t *testing.T) {
	sessions := &fakeSessions{}
	hub, _, _ := newContextHub(t, sessions)
	ctx := context.Background()

	const viewers = 50
	subscriptions := make([]*Subscription, 0, viewers)
	for index := 0; index < viewers; index++ {
		viewer, err := hub.Subscribe(ctx, "agent-1")
		if err != nil {
			t.Fatalf("subscribe viewer %d: %v", index, err)
		}
		subscriptions = append(subscriptions, viewer)
	}
	if got := sessions.openCount(); got != 1 {
		t.Fatalf("%d viewers opened %d Agent streams, want 1", viewers, got)
	}

	// Every viewer sees the same round.
	sessions.current().push(sampleFrame("a"))
	for index, viewer := range subscriptions {
		if rows := flatContainers(nextView(t, viewer)); len(rows) != 1 {
			t.Fatalf("viewer %d saw %d rows", index, len(rows))
		}
	}

	for index, viewer := range subscriptions {
		if err := viewer.Close(); err != nil {
			t.Fatalf("close viewer %d: %v", index, err)
		}
		if index < viewers-1 && sessions.closeCount() != 0 {
			t.Fatalf("the Agent stream closed while %d viewers were still watching", viewers-index-1)
		}
	}
	waitFor(t, "the Agent stream to close with the last viewer", func() bool { return sessions.closeCount() == 1 })
	if got := hub.relayCount(); got != 0 {
		t.Fatalf("%d relays survived their viewers", got)
	}
}

// A subscription holds one frame, never a queue. A viewer that never reads must
// cost a bounded amount of memory no matter how long it stays away, which is
// what keeps metrics from pushing Audit and control traffic behind them.
func TestASubscriptionHoldsOneFrameNoMatterHowManyArrive(t *testing.T) {
	sessions := &fakeSessions{}
	hub, _, _ := newContextHub(t, sessions)
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const rounds = 500
	stream := sessions.current()
	for round := 0; round < rounds; round++ {
		frame := sampleFrame("a")
		frame.Workload.ContainersTotal = uint32(round)
		stream.push(frame)
	}
	waitFor(t, "every round to be published", func() bool { return viewer.DroppedViews() == rounds-1 })

	// One frame is held, and it is the newest one - not the oldest, and not a
	// backlog of the ones in between.
	view := nextView(t, viewer)
	if view.Host.ContainersTotal != rounds-1 {
		t.Fatalf("the held frame is round %d, want the newest (%d)", view.Host.ContainersTotal, rounds-1)
	}
	if view.ViewerDropped != rounds-1 {
		t.Fatalf("the viewer was told it dropped %d of %d rounds", view.ViewerDropped, rounds)
	}
	// Nothing is left behind it.
	empty, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := viewer.Next(empty); err == nil {
		t.Fatal("a second frame was waiting behind the newest one")
	}
}

// Subscribing and unsubscribing repeatedly must not accumulate relays,
// goroutines, or Agent streams. Browsers open and close these views constantly;
// the cost of doing so has to come back to where it started.
func TestViewerChurnReturnsToBaseline(t *testing.T) {
	sessions := &fakeSessions{}
	hub, _, _ := newContextHub(t, sessions)
	ctx := context.Background()

	// Warm the paths before measuring, so the baseline is the steady state
	// rather than the first-run cost.
	for round := 0; round < 3; round++ {
		viewer, err := hub.Subscribe(ctx, "agent-1")
		if err != nil {
			t.Fatalf("warm subscribe: %v", err)
		}
		_ = viewer.Close()
	}
	waitFor(t, "the warm-up relays to be released", func() bool { return hub.relayCount() == 0 })
	baseline := settledGoroutines(t, 0)

	const iterations = 200
	for round := 0; round < iterations; round++ {
		viewer, err := hub.Subscribe(ctx, "agent-1")
		if err != nil {
			t.Fatalf("subscribe on round %d: %v", round, err)
		}
		if stream := sessions.current(); stream != nil {
			stream.push(sampleFrame("a"))
		}
		if err := viewer.Close(); err != nil {
			t.Fatalf("close on round %d: %v", round, err)
		}
	}

	waitFor(t, "every relay to be released", func() bool { return hub.relayCount() == 0 })
	waitFor(t, "every Agent stream to be closed", func() bool {
		return sessions.closeCount() == sessions.openCount()
	})
	if settled := settledGoroutines(t, baseline+10); settled > baseline+10 {
		t.Fatalf("goroutines after %d subscribe/close cycles = %d, baseline %d", iterations, settled, baseline)
	}
}

// One stalled viewer must not slow the ones keeping up, and must not stop the
// relay from advancing. A browser left open on a background tab is the normal
// case, not an incident.
func TestAStalledViewerDoesNotHoldUpTheOthers(t *testing.T) {
	sessions := &fakeSessions{}
	hub, _, _ := newContextHub(t, sessions)
	ctx := context.Background()

	stalled, err := hub.Subscribe(ctx, "agent-1")
	if err != nil {
		t.Fatalf("subscribe stalled viewer: %v", err)
	}
	defer stalled.Close()
	readers := make([]*Subscription, 0, 3)
	for index := 0; index < 3; index++ {
		viewer, err := hub.Subscribe(ctx, "agent-1")
		if err != nil {
			t.Fatalf("subscribe reader %d: %v", index, err)
		}
		defer viewer.Close()
		readers = append(readers, viewer)
	}

	const rounds = 20
	stream := sessions.current()
	for round := 0; round < rounds; round++ {
		frame := sampleFrame("a")
		frame.Workload.ContainersTotal = uint32(round)
		stream.push(frame)
		for index, viewer := range readers {
			view := nextView(t, viewer)
			if view.ViewerDropped != 0 {
				t.Fatalf("reader %d dropped %d rounds while another viewer stalled", index, view.ViewerDropped)
			}
			if view.Host.ContainersTotal != uint32(round) {
				t.Fatalf("reader %d received round %d, want %d", index, view.Host.ContainersTotal, round)
			}
		}
	}
	if got := stalled.DroppedViews(); got != rounds-1 {
		t.Fatalf("the stalled viewer counted %d dropped rounds of %d", got, rounds)
	}
	if got := sessions.openCount(); got != 1 {
		t.Fatalf("four viewers opened %d Agent streams", got)
	}
}
