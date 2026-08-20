package livematrix

import (
	"context"
	"testing"
	"time"
)

// framesHeld reports whether the subscription is holding a frame, which is the
// only buffer a viewer has. A queue would show up here as something this
// package does not have a way to express.
func (s *Subscription) framesHeld() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.has {
		return 1
	}
	return 0
}

// The Agent's own drop accounting, measured at the producer rather than
// inferred from what the Server saw downstream.
//
// A relay that outruns its consumer must hold exactly one frame - the newest -
// count every round the consumer missed, and never accumulate. This is the
// Agent half of the two-sided accounting: what is counted here is the Agent
// discarding rounds because the Server was slow, which is a different failure
// from the Server discarding rounds because a browser was slow, and the two
// counts are never combined.
func TestAgentProducerHoldsOneFrameAndCountsEveryRoundMissed(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "the first reconcile", func() bool { return h.membership.callCount() >= 1 })

	// Produce many rounds with nobody reading.
	//
	// Each round is waited for by its own effect, because "a frame is held" is
	// true from the first round onwards and would wave every later round
	// through without observing it. From round one on, the observable effect of
	// a round landing is the drop counter advancing - the round before it was
	// discarded to make room - and that is what is waited for.
	const produced = 100
	for round := 0; round < produced; round++ {
		h.tickers.tick(t)
		if round == 0 {
			waitFor(t, "the first round to land", func() bool { return viewer.framesHeld() == 1 })
		} else {
			want := uint64(round)
			waitFor(t, "the round to land", func() bool { return viewer.DroppedFrames() >= want })
		}
		if held := viewer.framesHeld(); held != 1 {
			t.Fatalf("the subscription held %d frames after round %d, want exactly one", held, round)
		}
	}

	dropped := viewer.DroppedFrames()
	if dropped != produced-1 {
		t.Fatalf("the producer counted %d dropped rounds of %d produced, want %d",
			dropped, produced, produced-1)
	}

	// The one frame held is a whole frame, not a fragment of the rounds that
	// were coalesced into it.
	frame := nextFrame(t, viewer)
	if len(frame.Rows) != 1 || frame.Rows[0].ContainerID != "a" {
		t.Fatalf("the surviving frame is incomplete: %+v", frame.Rows)
	}
	delivered := 1

	// The accounting closes: every round produced was either delivered or
	// counted as dropped, exactly once. This is the invariant, not a particular
	// amount of coalescing.
	if uint64(delivered)+dropped != produced {
		t.Fatalf("%d delivered and %d dropped does not account for the %d rounds produced",
			delivered, dropped, produced)
	}

	// Nothing is queued behind what was just read.
	if held := viewer.framesHeld(); held != 0 {
		t.Fatalf("the subscription still holds %d frames after being read", held)
	}
	empty, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := viewer.Next(empty); err == nil {
		t.Fatal("a second frame was waiting behind the newest one")
	}
}

// A consumer that keeps up drops nothing, which is what makes the count above
// mean something rather than being an artefact of how frames are handed over.
func TestAConsumerThatKeepsUpDropsNothing(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	viewer, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "the first reconcile", func() bool { return h.membership.callCount() >= 1 })

	const rounds = 20
	for round := 0; round < rounds; round++ {
		h.tickers.tick(t)
		if frame := nextFrame(t, viewer); len(frame.Rows) != 1 {
			t.Fatalf("round %d delivered %d rows", round, len(frame.Rows))
		}
	}
	if dropped := viewer.DroppedFrames(); dropped != 0 {
		t.Fatalf("a consumer that read every round was told it dropped %d", dropped)
	}
}

// Two viewers account separately. A slow one must not make a keeping-up one
// look behind, and the relay produces one round for both regardless.
func TestEachViewerAccountsForItself(t *testing.T) {
	source := &statsSource{}
	h := newHarness(t, source, "a")
	quick, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer quick.Close()
	slow, err := h.hub.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	waitFor(t, "the first reconcile", func() bool { return h.membership.callCount() >= 1 })

	const rounds = 10
	for round := 0; round < rounds; round++ {
		h.tickers.tick(t)
		// The quick viewer reading is the proof the round was published; the
		// slow one is charged for it in the same publish.
		nextFrame(t, quick)
		if round > 0 {
			want := uint64(round)
			waitFor(t, "the slow viewer to be charged", func() bool { return slow.DroppedFrames() >= want })
		}
	}
	if dropped := quick.DroppedFrames(); dropped != 0 {
		t.Fatalf("the viewer that kept up was charged %d dropped rounds", dropped)
	}
	if dropped := slow.DroppedFrames(); dropped != rounds-1 {
		t.Fatalf("the slow viewer counted %d dropped rounds of %d", dropped, rounds)
	}
	if held := slow.framesHeld(); held != 1 {
		t.Fatalf("the slow viewer accumulated %d frames", held)
	}
}
