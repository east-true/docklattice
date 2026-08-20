// Package livestats provides viewer-scoped, non-persistent live statistics
// relaying. It deliberately owns no Docker SDK types.
package livestats

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultSampleInterval = 2 * time.Second
	MaxBrowserSamples     = 120
)

var (
	ErrClosed      = errors.New("live stats hub is closed")
	ErrSourceEnded = errors.New("Docker stats stream ended")
)

type Sample struct {
	ContainerID  string
	ObservedAt   time.Time
	CPUPercent   float64
	MemoryUsage  uint64
	MemoryLimit  uint64
	NetworkRX    uint64
	NetworkTX    uint64
	BlockRead    uint64
	BlockWrite   uint64
	RestartCount uint64
	Health       string
	Uptime       time.Duration
}

// Source is the narrow Docker-adapter integration boundary. Stream must keep
// one Docker stats stream open until ctx is canceled or the Engine fails.
type Source interface {
	Stream(context.Context, string, func(Sample) error) error
}

type SourceFunc func(context.Context, string, func(Sample) error) error

func (f SourceFunc) Stream(ctx context.Context, containerID string, emit func(Sample) error) error {
	return f(ctx, containerID, emit)
}

type Clock interface{ Now() time.Time }

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TickerFactory interface{ NewTicker(time.Duration) Ticker }

type TickerFactoryFunc func(time.Duration) Ticker

func (f TickerFactoryFunc) NewTicker(interval time.Duration) Ticker { return f(interval) }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type realTicker struct{ ticker *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

type realTickerFactory struct{}

func (realTickerFactory) NewTicker(interval time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(interval)}
}

type Config struct {
	Source         Source
	SampleInterval time.Duration
	Clock          Clock
	TickerFactory  TickerFactory
}

type containerRelay struct {
	id        string
	ctx       context.Context
	cancel    context.CancelFunc
	viewers   map[uint64]*Subscription
	latest    Sample
	version   uint64
	published uint64
}

type Hub struct {
	source   Source
	interval time.Duration
	clock    Clock
	tickers  TickerFactory

	mu         sync.Mutex
	closed     bool
	nextViewer uint64
	containers map[string]*containerRelay
}

func New(config Config) (*Hub, error) {
	if config.Source == nil {
		return nil, errors.New("stats source is required")
	}
	if config.SampleInterval == 0 {
		config.SampleInterval = DefaultSampleInterval
	}
	if config.SampleInterval <= 0 {
		return nil, errors.New("stats sample interval must be positive")
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.TickerFactory == nil {
		config.TickerFactory = realTickerFactory{}
	}
	return &Hub{
		source: config.Source, interval: config.SampleInterval, clock: config.Clock,
		tickers: config.TickerFactory, containers: make(map[string]*containerRelay),
	}, nil
}

// Subscribe starts the sole Docker stats stream for containerID on the first
// viewer. The returned subscription retains only its newest unread sample.
func (h *Hub) Subscribe(ctx context.Context, containerID string) (*Subscription, error) {
	if ctx == nil {
		return nil, errors.New("viewer context is required")
	}
	if containerID == "" {
		return nil, errors.New("container ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	relay := h.containers[containerID]
	first := relay == nil
	if first {
		relayCtx, cancel := context.WithCancel(context.Background())
		relay = &containerRelay{id: containerID, ctx: relayCtx, cancel: cancel, viewers: make(map[uint64]*Subscription)}
		h.containers[containerID] = relay
	}
	h.nextViewer++
	sub := &Subscription{
		hub: h, relay: relay, id: h.nextViewer, notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	relay.viewers[sub.id] = sub
	if relay.published != 0 {
		sub.put(relay.latest)
	}
	h.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			_ = sub.Close()
		case <-sub.Done():
		}
	}()
	if first {
		go h.collect(relay)
		go h.sample(relay)
	}
	return sub, nil
}

func (h *Hub) collect(relay *containerRelay) {
	err := h.source.Stream(relay.ctx, relay.id, func(sample Sample) error {
		if err := relay.ctx.Err(); err != nil {
			return err
		}
		if sample.ContainerID != "" && sample.ContainerID != relay.id {
			return fmt.Errorf("stats source returned container %q for %q", sample.ContainerID, relay.id)
		}
		sample.ContainerID = relay.id
		if sample.ObservedAt.IsZero() {
			sample.ObservedAt = h.clock.Now()
		}
		h.mu.Lock()
		if h.containers[relay.id] == relay {
			relay.latest = sample
			relay.version++
		}
		h.mu.Unlock()
		return nil
	})
	if err == nil && relay.ctx.Err() == nil {
		err = ErrSourceEnded
	}
	h.endRelay(relay, err)
}

func (h *Hub) sample(relay *containerRelay) {
	ticker := h.tickers.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-relay.ctx.Done():
			return
		case <-ticker.C():
			h.publish(relay)
		}
	}
}

func (h *Hub) publish(relay *containerRelay) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.containers[relay.id] != relay || relay.version == 0 || relay.version == relay.published {
		return
	}
	relay.published = relay.version
	for _, viewer := range relay.viewers {
		viewer.put(relay.latest)
	}
}

func (h *Hub) endRelay(relay *containerRelay, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.containers[relay.id] != relay {
		return
	}
	delete(h.containers, relay.id)
	relay.cancel()
	for _, viewer := range relay.viewers {
		viewer.finish(err)
	}
}

func (h *Hub) unsubscribe(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	relay := sub.relay
	if _, ok := relay.viewers[sub.id]; !ok {
		return
	}
	delete(relay.viewers, sub.id)
	sub.finish(nil)
	if len(relay.viewers) == 0 && h.containers[relay.id] == relay {
		delete(h.containers, relay.id)
		relay.cancel()
	}
}

func (h *Hub) ActiveStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.containers)
}

func (h *Hub) ViewerCount(containerID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if relay := h.containers[containerID]; relay != nil {
		return len(relay.viewers)
	}
	return 0
}

func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for id, relay := range h.containers {
		delete(h.containers, id)
		relay.cancel()
		for _, viewer := range relay.viewers {
			viewer.finish(ErrClosed)
		}
	}
	return nil
}

type Subscription struct {
	hub    *Hub
	relay  *containerRelay
	id     uint64
	notify chan struct{}
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	latest  Sample
	has     bool
	everHad bool
	err     error
	dropped atomic.Uint64
}

func (s *Subscription) put(sample Sample) {
	s.mu.Lock()
	if s.has {
		s.dropped.Add(1)
	}
	s.latest = sample
	s.has = true
	s.everHad = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Subscription) Next(ctx context.Context) (Sample, error) {
	if ctx == nil {
		return Sample{}, errors.New("Next context is required")
	}
	for {
		s.mu.Lock()
		if s.has {
			sample := s.latest
			s.has = false
			s.mu.Unlock()
			return sample, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Sample{}, ctx.Err()
		case <-s.done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err != nil {
				return Sample{}, err
			}
			return Sample{}, io.EOF
		case <-s.notify:
		}
	}
}

// Latest reports the newest sample without consuming it, and says whether one
// has arrived at all. Frame assembly needs this: a matrix frame is built from
// every watched container at once, and a container whose first sample has not
// arrived yet must leave its row pending rather than hold up the other two
// hundred. Next stays the consuming read for single-container viewers.
func (s *Subscription) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.has || s.everHad
}

func (s *Subscription) DroppedSamples() uint64 { return s.dropped.Load() }
func (s *Subscription) Done() <-chan struct{}  { return s.done }
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *Subscription) Close() error {
	s.hub.unsubscribe(s)
	return nil
}
func (s *Subscription) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

// LatestStore is the complete Server-side stats cache: exactly one sample per
// active container and no history or persistence hooks.
type LatestStore struct {
	mu     sync.RWMutex
	latest map[string]Sample
}

func NewLatestStore() *LatestStore { return &LatestStore{latest: make(map[string]Sample)} }

func (s *LatestStore) Update(sample Sample) error {
	if sample.ContainerID == "" {
		return errors.New("container ID is required")
	}
	s.mu.Lock()
	s.latest[sample.ContainerID] = sample
	s.mu.Unlock()
	return nil
}

func (s *LatestStore) Get(containerID string) (Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample, ok := s.latest[containerID]
	return sample, ok
}

func (s *LatestStore) Deactivate(containerID string) {
	s.mu.Lock()
	delete(s.latest, containerID)
	s.mu.Unlock()
}

func (s *LatestStore) ActiveContainers() []string {
	s.mu.RLock()
	ids := make([]string, 0, len(s.latest))
	for id := range s.latest {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// BrowserRing models the browser-owned, refresh-ephemeral sparkline buffer.
// It cannot be configured above the product limit of 120 samples.
type BrowserRing struct {
	mu          sync.Mutex
	containerID string
	capacity    int
	samples     []Sample
	next        int
	full        bool
}

func NewBrowserRing(containerID string, capacity int) (*BrowserRing, error) {
	if containerID == "" {
		return nil, errors.New("container ID is required")
	}
	if capacity == 0 {
		capacity = MaxBrowserSamples
	}
	if capacity < 1 || capacity > MaxBrowserSamples {
		return nil, fmt.Errorf("browser stats capacity must be between 1 and %d", MaxBrowserSamples)
	}
	return &BrowserRing{containerID: containerID, capacity: capacity, samples: make([]Sample, capacity)}, nil
}

func (r *BrowserRing) Add(sample Sample) error {
	if sample.ContainerID != r.containerID {
		return fmt.Errorf("sample container %q does not match ring container %q", sample.ContainerID, r.containerID)
	}
	r.mu.Lock()
	r.samples[r.next] = sample
	r.next = (r.next + 1) % r.capacity
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
	return nil
}

func (r *BrowserRing) Samples() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]Sample(nil), r.samples[:r.next]...)
	}
	result := make([]Sample, 0, r.capacity)
	result = append(result, r.samples[r.next:]...)
	result = append(result, r.samples[:r.next]...)
	return result
}
