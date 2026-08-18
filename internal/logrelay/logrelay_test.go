package logrelay

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestPerStreamByteRateReportsDrops(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	relay, err := New(Config{
		BytesPerSecond: 5, MaxBufferedBytes: 100, MaxBufferedChunks: 10, Clock: clock,
		Source: SourceFunc(func(_ context.Context, _ Request, emit func(Chunk) error) error {
			_ = emit(Chunk{Data: []byte("aaaa\n"), LineCount: 1})
			_ = emit(Chunk{Data: []byte("bbbb\n"), LineCount: 1})
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := relay.Open(context.Background(), Request{ContainerID: "container", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Done()
	events := drainEvents(t, stream)
	var sent, dropped uint64
	for _, event := range events {
		sent += uint64(len(event.Data))
		dropped += event.DroppedBytes
	}
	if sent != 5 || dropped != 5 {
		t.Fatalf("sent=%d dropped=%d events=%+v", sent, dropped, events)
	}
}

func TestSlowConsumerBufferDropsOldestWithExplicitCounts(t *testing.T) {
	relay, err := New(Config{
		BytesPerSecond: 1 << 20, MaxBufferedBytes: 6, MaxBufferedChunks: 2,
		Source: SourceFunc(func(_ context.Context, _ Request, emit func(Chunk) error) error {
			for _, data := range []string{"a1\n", "b2\n", "c3\n"} {
				_ = emit(Chunk{Data: []byte(data), LineCount: 1})
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := relay.Open(context.Background(), Request{ContainerID: "container"})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Done()
	bytes, chunks := stream.Buffered()
	if bytes != 6 || chunks != 2 {
		t.Fatalf("buffer bytes=%d chunks=%d", bytes, chunks)
	}
	events := drainEvents(t, stream)
	if len(events) != 3 || string(events[0].Data) != "b2\n" || events[0].DroppedBytes != 3 || events[0].DroppedLines != 1 ||
		string(events[1].Data) != "c3\n" || !events[2].Terminal {
		t.Fatalf("events = %+v", events)
	}
}

func TestOversizedChunkProducesDropOnlyTerminalEvent(t *testing.T) {
	relay, err := New(Config{
		BytesPerSecond: 100, MaxBufferedBytes: 4, MaxBufferedChunks: 2,
		Source: SourceFunc(func(_ context.Context, _ Request, emit func(Chunk) error) error {
			return emit(Chunk{Data: []byte("too-large\n"), LineCount: 1})
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := relay.Open(context.Background(), Request{ContainerID: "container"})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Done()
	event, err := stream.Next(context.Background())
	if err != nil || !event.Terminal || event.DroppedBytes != uint64(len("too-large\n")) || event.DroppedLines != 1 {
		t.Fatalf("terminal drop = %+v, %v", event, err)
	}
}

func TestBrowserCancellationPropagatesAndDoesNotResume(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var calls int
	var mu sync.Mutex
	relay, err := New(Config{
		Source: SourceFunc(func(ctx context.Context, _ Request, _ func(Chunk) error) error {
			mu.Lock()
			calls++
			mu.Unlock()
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := relay.Open(ctx, Request{ContainerID: "container", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("browser cancellation did not reach log source")
	}
	<-stream.Done()
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("stream error = %v", stream.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("source calls = %d; stream auto-resumed", calls)
	}
}

func TestByteLimiterRefillsWithoutExceedingOneSecondBurst(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	limiter := newByteLimiter(10, clock)
	if !limiter.Allow(10) || limiter.Allow(1) {
		t.Fatal("initial rate bucket was not bounded")
	}
	clock.Advance(500 * time.Millisecond)
	if !limiter.Allow(5) || limiter.Allow(1) {
		t.Fatal("half-second refill was not exact")
	}
	clock.Advance(10 * time.Second)
	if !limiter.Allow(10) || limiter.Allow(1) {
		t.Fatal("rate bucket exceeded one-second burst")
	}
}

func drainEvents(t *testing.T, stream *Stream) []Event {
	t.Helper()
	var events []Event
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}
