package livestats

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeTicker struct {
	ticks chan time.Time
	done  chan struct{}
	once  sync.Once
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ticks: make(chan time.Time), done: make(chan struct{})}
}

func (t *fakeTicker) C() <-chan time.Time { return t.ticks }
func (t *fakeTicker) Stop()               { t.once.Do(func() { close(t.done) }) }

type fakeStatsSource struct {
	mu       sync.Mutex
	emitters map[string]func(Sample) error
	starts   map[string]int
	started  chan string
	stopped  chan string
}

func newFakeStatsSource() *fakeStatsSource {
	return &fakeStatsSource{
		emitters: make(map[string]func(Sample) error), starts: make(map[string]int),
		started: make(chan string, 10), stopped: make(chan string, 10),
	}
}

func (s *fakeStatsSource) Stream(ctx context.Context, containerID string, emit func(Sample) error) error {
	s.mu.Lock()
	s.emitters[containerID] = emit
	s.starts[containerID]++
	s.mu.Unlock()
	s.started <- containerID
	<-ctx.Done()
	s.stopped <- containerID
	return ctx.Err()
}

func (s *fakeStatsSource) emit(containerID string, sample Sample) error {
	s.mu.Lock()
	emit := s.emitters[containerID]
	s.mu.Unlock()
	return emit(sample)
}

func (s *fakeStatsSource) startCount(containerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts[containerID]
}

func TestViewerScopedSingleStreamLatestWinsAndCancellation(t *testing.T) {
	source := newFakeStatsSource()
	ticker := newFakeTicker()
	hub, err := New(Config{
		Source: source, SampleInterval: DefaultSampleInterval,
		TickerFactory: TickerFactoryFunc(func(interval time.Duration) Ticker {
			if interval != 2*time.Second {
				t.Errorf("sample interval = %s", interval)
			}
			return ticker
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if hub.ActiveStreams() != 0 {
		t.Fatal("zero viewers started a Docker stream")
	}
	first, err := hub.Subscribe(context.Background(), "container-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Subscribe(context.Background(), "container-1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("first viewer did not start Docker stream")
	}
	if source.startCount("container-1") != 1 || hub.ActiveStreams() != 1 || hub.ViewerCount("container-1") != 2 {
		t.Fatalf("starts=%d streams=%d viewers=%d", source.startCount("container-1"), hub.ActiveStreams(), hub.ViewerCount("container-1"))
	}
	for cpu := 1.0; cpu <= 3; cpu++ {
		if err := source.emit("container-1", Sample{CPUPercent: cpu}); err != nil {
			t.Fatal(err)
		}
	}
	ticker.ticks <- time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, viewer := range []*Subscription{first, second} {
		sample, err := viewer.Next(ctx)
		if err != nil || sample.CPUPercent != 3 || sample.ContainerID != "container-1" {
			t.Fatalf("sample = %+v, %v", sample, err)
		}
	}

	if err := source.emit("container-1", Sample{CPUPercent: 4}); err != nil {
		t.Fatal(err)
	}
	ticker.ticks <- time.Now()
	waitForPendingCPU(t, first, 4)
	if err := source.emit("container-1", Sample{CPUPercent: 5}); err != nil {
		t.Fatal(err)
	}
	ticker.ticks <- time.Now()
	waitForPendingCPU(t, first, 5)
	sample, err := first.Next(ctx)
	if err != nil || sample.CPUPercent != 5 || first.DroppedSamples() != 1 {
		t.Fatalf("viewer latest = %+v drops=%d err=%v", sample, first.DroppedSamples(), err)
	}
	_ = first.Close()
	if hub.ActiveStreams() != 1 || hub.ViewerCount("container-1") != 1 {
		t.Fatal("non-final viewer canceled shared Docker stream")
	}
	_ = second.Close()
	select {
	case <-source.stopped:
	case <-time.After(time.Second):
		t.Fatal("last viewer did not cancel Docker stream")
	}
	if hub.ActiveStreams() != 0 {
		t.Fatal("stream remained active after last viewer")
	}
}

func waitForPendingCPU(t *testing.T, subscription *Subscription, cpu float64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		subscription.mu.Lock()
		ready := subscription.has && subscription.latest.CPUPercent == cpu
		subscription.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("viewer did not receive pending CPU sample %v", cpu)
}

func TestViewerContextCancellationPropagatesToSource(t *testing.T) {
	source := newFakeStatsSource()
	ticker := newFakeTicker()
	hub, err := New(Config{Source: source, TickerFactory: TickerFactoryFunc(func(time.Duration) Ticker { return ticker })})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = hub.Subscribe(ctx, "container")
	if err != nil {
		t.Fatal(err)
	}
	<-source.started
	cancel()
	select {
	case <-source.stopped:
	case <-time.After(time.Second):
		t.Fatal("viewer cancellation did not reach source")
	}
}

func TestLatestStoreAndBrowserRingHaveHardHistoryBounds(t *testing.T) {
	store := NewLatestStore()
	for cpu := 1.0; cpu <= 3; cpu++ {
		if err := store.Update(Sample{ContainerID: "container", CPUPercent: cpu}); err != nil {
			t.Fatal(err)
		}
	}
	latest, ok := store.Get("container")
	if !ok || latest.CPUPercent != 3 || len(store.ActiveContainers()) != 1 {
		t.Fatalf("latest = %+v, %v", latest, ok)
	}
	store.Deactivate("container")
	if _, ok := store.Get("container"); ok {
		t.Fatal("inactive container remained cached")
	}

	ring, err := NewBrowserRing("container", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 125; i++ {
		if err := ring.Add(Sample{ContainerID: "container", CPUPercent: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	samples := ring.Samples()
	if len(samples) != MaxBrowserSamples || samples[0].CPUPercent != 5 || samples[len(samples)-1].CPUPercent != 124 {
		t.Fatalf("ring len=%d first=%v last=%v", len(samples), samples[0].CPUPercent, samples[len(samples)-1].CPUPercent)
	}
	if _, err := NewBrowserRing("container", MaxBrowserSamples+1); err == nil {
		t.Fatal("browser ring accepted more than 120 samples")
	}
}
