// Package livematrix assembles one host's live metrics into whole frames.
//
// It is not a second metrics subsystem. Container sampling stays in livestats,
// which already shares one Docker stats stream between viewers and keeps only
// the newest sample; this package decides which containers belong in a frame,
// puts the current value of every one of them into a single frame with the
// workload summary, and hands frames to viewers.
//
// The frame is the unit for a reason. Multiplexing per-container samples onto
// one latest-wins stream would let a busy container overwrite a quiet one
// indefinitely. Losing a whole frame loses one round of everything, and the
// next frame carries every container again.
package livematrix

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/east-true/dockpilot/internal/livestats"
)

var ErrClosed = errors.New("live metrics matrix is closed")

// Membership is the set of containers a frame covers, as the Docker Engine sees
// it. Discovery metadata is joined later by the Server; a container missing from
// discovery is still a running container and still belongs in the frame.
type Membership interface {
	// Running lists the container IDs currently running on this host.
	Running(context.Context) ([]string, error)
}

// Events reports container lifecycle changes so membership follows Docker
// promptly instead of waiting for the next reconcile. It is separate from
// Membership because a dropped event must degrade a frame, never the view:
// reconcile repairs whatever the event stream missed.
type Events interface {
	// Watch delivers container IDs whose lifecycle changed until ctx ends.
	// Which way it changed is not reported, because the answer is always the
	// same: ask Membership.
	Watch(ctx context.Context, changed func()) error
}

// Workload reports the host row: the capacity this Engine has and the paths
// Dockpilot writes to. It is deliberately not host OS metrics - an Agent runs
// in a container, where /proc/net belongs to the container's own network
// namespace.
type Workload interface {
	Capacity(context.Context) (Capacity, error)
}

type Capacity struct {
	CPUCapacity     uint32
	MemoryCapacity  uint64
	ContainersTotal uint32
	Filesystems     []Filesystem
}

// Filesystem is one managed path's capacity. Unavailable says this particular
// filesystem could not be read - a discovery root that has gone, or one the
// Agent cannot stat - which is a fact about that path and not about the host.
// One unreadable root must not take the whole workload summary with it.
type Filesystem struct {
	Path        string
	TotalBytes  uint64
	FreeBytes   uint64
	Unavailable bool
	Reason      string
}

// Row is one container in a frame. Pending is true when the container is in the
// membership snapshot but its first sample has not arrived; the row is still
// present, because a container that exists and has not reported yet is a
// different thing from a container that is gone.
type Row struct {
	ContainerID string
	Pending     bool
	Sample      livestats.Sample
}

// Frame is one host at one instant. Rows and Running come from a single
// membership snapshot, so a frame never disagrees with itself.
//
// MembershipStale says the rows are the last known set rather than the current
// one, because the attempt to refresh them from Docker failed. Keeping the rows
// and saying nothing would assert they are current; dropping them would say the
// host has no containers. Neither is true: a failed listing means membership is
// unknown, which is a third thing and is what this reports.
//
// Capacity is the Engine's own numbers and changes rarely; a frame carries the
// last successful read of it.
type Frame struct {
	ObservedAt       time.Time
	Capacity         Capacity
	Running          uint32
	Rows             []Row
	MembershipStale  bool
	MembershipReason string
	// WorkloadStale is the Engine's own capacity numbers failing to refresh. It
	// moves independently of MembershipStale: listing containers and asking the
	// Engine about itself are different calls that fail for different reasons,
	// and collapsing them into one frame-wide error would report a stale CPU
	// count as containers of unknown membership, or the reverse.
	WorkloadStale  bool
	WorkloadReason string
}

type Clock interface{ Now() time.Time }

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TickerFactory interface{ NewTicker(time.Duration) Ticker }

type Config struct {
	Stats *livestats.Hub
	// Membership, Events and Workload are the Docker-facing boundary.
	Membership Membership
	Events     Events
	Workload   Workload
	// FrameInterval is both the frame cadence and the reconcile cadence. One
	// tick does both, so a reconcile can never race the frame it repairs.
	FrameInterval time.Duration
	// ReconcileEvery is how many frame ticks pass between full membership
	// reconciles. Events carry the common case; this bounds the cost of the
	// repair that catches what they missed.
	ReconcileEvery int
	Clock          Clock
	TickerFactory  TickerFactory
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type realTicker struct{ ticker *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

type realTickerFactory struct{}

func (realTickerFactory) NewTicker(interval time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(interval)}
}

// Hub owns the single host relay. Every viewer of this host shares it: the
// membership set, the Docker event subscription, the reconcile ticker and the
// per-container subscriptions all exist exactly once, from the first viewer
// until the last one leaves.
type Hub struct {
	config Config

	mu         sync.Mutex
	closed     bool
	nextViewer uint64
	relay      *hostRelay
}

type hostRelay struct {
	ctx     context.Context
	cancel  context.CancelFunc
	viewers map[uint64]*Subscription

	// members maps container ID to its livestats subscription. It is the
	// desired membership, and add/remove are idempotent so an event and a
	// reconcile arriving about the same container cannot start two relays or
	// close one twice.
	members  map[string]*livestats.Subscription
	capacity Capacity
	haveCap  bool

	// staleReason is empty when the last membership refresh succeeded. It is
	// what turns "these rows are current" into "these rows are the last ones we
	// could confirm, and here is why". workloadReason is the same fact about
	// the Engine's capacity, kept separately because the two calls fail apart.
	staleReason    string
	workloadReason string
}

func New(config Config) (*Hub, error) {
	if config.Stats == nil {
		return nil, errors.New("livematrix: stats hub is required")
	}
	if config.Membership == nil {
		return nil, errors.New("livematrix: membership source is required")
	}
	if config.Workload == nil {
		return nil, errors.New("livematrix: workload source is required")
	}
	if config.FrameInterval <= 0 {
		return nil, errors.New("livematrix: frame interval must be positive")
	}
	if config.ReconcileEvery <= 0 {
		config.ReconcileEvery = 15
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.TickerFactory == nil {
		config.TickerFactory = realTickerFactory{}
	}
	return &Hub{config: config}, nil
}

// Subscribe attaches a viewer. The first one starts collection; the rest share
// everything it started.
func (h *Hub) Subscribe(ctx context.Context) (*Subscription, error) {
	if ctx == nil {
		return nil, errors.New("livematrix: viewer context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	relay := h.relay
	started := false
	if relay == nil {
		relayCtx, cancel := context.WithCancel(context.Background())
		relay = &hostRelay{
			ctx: relayCtx, cancel: cancel,
			viewers: make(map[uint64]*Subscription),
			members: make(map[string]*livestats.Subscription),
		}
		h.relay = relay
		started = true
	}
	h.nextViewer++
	viewer := &Subscription{
		hub: h, relay: relay, id: h.nextViewer,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	relay.viewers[viewer.id] = viewer
	h.mu.Unlock()

	if started {
		go h.run(relay)
	}
	return viewer, nil
}

// run owns everything the relay started, and unwinds all of it on the way out.
func (h *Hub) run(relay *hostRelay) {
	defer h.stopRelay(relay)

	// A first reconcile before any frame, so the opening frame describes the
	// host rather than an empty set.
	h.reconcile(relay, true)

	changed := make(chan struct{}, 1)
	if h.config.Events != nil {
		go func() {
			_ = h.config.Events.Watch(relay.ctx, func() {
				select {
				case changed <- struct{}{}:
				default:
				}
			})
		}()
	}

	ticker := h.config.TickerFactory.NewTicker(h.config.FrameInterval)
	defer ticker.Stop()
	sinceReconcile := 0
	for {
		select {
		case <-relay.ctx.Done():
			return
		case <-changed:
			// An event says something moved. Ask Docker rather than trusting
			// the event to say what: one answer, one code path.
			//
			// Membership only. A container starting says nothing about how many
			// CPUs the host has, and a burst of starts must not turn into a
			// burst of Engine info calls; capacity refreshes on its own cadence
			// below.
			h.reconcile(relay, false)
		case <-ticker.C():
			sinceReconcile++
			if sinceReconcile >= h.config.ReconcileEvery {
				sinceReconcile = 0
				h.reconcile(relay, true)
			}
			h.publish(relay)
		}
	}
}

// reconcile brings the relay back in line with the host. Membership always;
// the workload summary only when asked, because host capacity changes on the
// timescale of rebooting a machine, not of starting a container.
//
// The two run one after the other and neither can skip the other: a Docker
// listing failure must not stop filesystem capacity from refreshing, and an
// Engine that cannot describe itself must not stop membership from moving.
func (h *Hub) reconcile(relay *hostRelay, refreshWorkload bool) {
	h.reconcileMembership(relay)
	if refreshWorkload {
		h.refreshWorkload(relay)
	}
}

// reconcileMembership makes the membership set match Docker. It is the only
// writer of relay.members, and it is idempotent: containers already present
// keep their existing subscription, and containers that left are closed
// exactly once.
func (h *Hub) reconcileMembership(relay *hostRelay) {
	running, err := h.config.Membership.Running(relay.ctx)
	if err != nil {
		// A failed listing is not an empty host. The previous membership stays,
		// and the frame says it is stale so nobody reads it as current. An
		// Engine that has never answered leaves no rows and the same reason,
		// which reads as "Docker unavailable" rather than "no containers".
		h.mu.Lock()
		if h.relay == relay {
			relay.staleReason = boundedReason(err)
		}
		h.mu.Unlock()
		return
	}
	desired := make(map[string]struct{}, len(running))
	for _, id := range running {
		if id != "" {
			desired[id] = struct{}{}
		}
	}

	h.mu.Lock()
	if h.relay != relay {
		h.mu.Unlock()
		return
	}
	relay.staleReason = ""
	var toClose []*livestats.Subscription
	for id, subscription := range relay.members {
		if _, keep := desired[id]; !keep {
			toClose = append(toClose, subscription)
			delete(relay.members, id)
		}
	}
	missing := make([]string, 0)
	for id := range desired {
		if _, present := relay.members[id]; !present {
			missing = append(missing, id)
		}
	}
	h.mu.Unlock()

	for _, subscription := range toClose {
		_ = subscription.Close()
	}
	for _, id := range missing {
		subscription, err := h.config.Stats.Subscribe(relay.ctx, id)
		if err != nil {
			// The container may have gone between listing and subscribing.
			// The next reconcile settles it.
			continue
		}
		h.mu.Lock()
		if h.relay != relay {
			h.mu.Unlock()
			_ = subscription.Close()
			return
		}
		if existing, present := relay.members[id]; present {
			// Another path already added it. Keep one, close the duplicate.
			h.mu.Unlock()
			_ = subscription.Close()
			_ = existing
			continue
		}
		relay.members[id] = subscription
		h.mu.Unlock()
	}

}

// refreshWorkload re-reads the host summary. A failure here leaves the last
// known capacity in place and marks only the workload half of the frame; the
// container rows are unaffected.
func (h *Hub) refreshWorkload(relay *hostRelay) {
	capacity, capacityErr := h.config.Workload.Capacity(relay.ctx)
	h.mu.Lock()
	if h.relay == relay {
		if capacityErr == nil {
			relay.capacity, relay.haveCap, relay.workloadReason = capacity, true, ""
		} else {
			relay.workloadReason = boundedReason(capacityErr)
		}
	}
	h.mu.Unlock()
}

// publish assembles one frame and hands it to every viewer.
//
// Membership is read once, under the lock, and the rows and the running count
// come from that one read. A container that dies while this runs leaves in the
// next frame; it never produces a frame whose summary and rows disagree.
func (h *Hub) publish(relay *hostRelay) {
	h.mu.Lock()
	if h.relay != relay {
		h.mu.Unlock()
		return
	}
	members := make([]string, 0, len(relay.members))
	subscriptions := make([]*livestats.Subscription, 0, len(relay.members))
	for id, subscription := range relay.members {
		members = append(members, id)
		subscriptions = append(subscriptions, subscription)
	}
	capacity := relay.capacity
	staleReason := relay.staleReason
	workloadReason := relay.workloadReason
	haveCapacity := relay.haveCap
	viewers := make([]*Subscription, 0, len(relay.viewers))
	for _, viewer := range relay.viewers {
		viewers = append(viewers, viewer)
	}
	h.mu.Unlock()

	rows := make([]Row, len(members))
	for index := range members {
		row := Row{ContainerID: members[index]}
		// Latest never blocks. A container whose first sample has not arrived
		// leaves its row pending rather than holding up the frame - one slow
		// container must not set the cadence for the other two hundred.
		if sample, ok := subscriptions[index].Latest(); ok {
			row.Sample = sample
		} else {
			row.Pending = true
		}
		rows[index] = row
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ContainerID < rows[j].ContainerID })

	frame := Frame{
		ObservedAt:       h.config.Clock.Now(),
		Capacity:         capacity,
		Running:          uint32(len(rows)),
		Rows:             rows,
		MembershipStale:  staleReason != "",
		MembershipReason: staleReason,
		WorkloadStale:    workloadReason != "" || !haveCapacity,
		WorkloadReason:   workloadReason,
	}
	for _, viewer := range viewers {
		viewer.put(frame)
	}
}

// boundedReason keeps an Engine error short enough to travel in every frame.
// It is a reason for an operator, not a payload.
func boundedReason(err error) string {
	const limit = 200
	message := err.Error()
	if message == "" {
		message = "Docker listing failed"
	}
	if len(message) > limit {
		message = message[:limit]
	}
	return message
}

func (h *Hub) unsubscribe(viewer *Subscription) {
	h.mu.Lock()
	relay := viewer.relay
	if relay == nil || h.relay != relay {
		h.mu.Unlock()
		viewer.finish(nil)
		return
	}
	delete(relay.viewers, viewer.id)
	last := len(relay.viewers) == 0
	if last {
		h.relay = nil
	}
	h.mu.Unlock()
	viewer.finish(nil)
	if last {
		// Cancelling the relay context ends the event watch, the ticker loop
		// and every container subscription's parent, and run's deferred
		// stopRelay closes the subscriptions themselves.
		relay.cancel()
	}
}

func (h *Hub) stopRelay(relay *hostRelay) {
	h.mu.Lock()
	if h.relay == relay {
		h.relay = nil
	}
	members := relay.members
	relay.members = make(map[string]*livestats.Subscription)
	viewers := make([]*Subscription, 0, len(relay.viewers))
	for _, viewer := range relay.viewers {
		viewers = append(viewers, viewer)
	}
	relay.viewers = make(map[uint64]*Subscription)
	h.mu.Unlock()

	relay.cancel()
	for _, subscription := range members {
		_ = subscription.Close()
	}
	for _, viewer := range viewers {
		viewer.finish(io.EOF)
	}
}

// Close ends every relay and every viewer.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	relay := h.relay
	h.relay = nil
	h.mu.Unlock()
	if relay != nil {
		relay.cancel()
	}
	return nil
}

// Subscription is one viewer's frames. It holds one frame: a viewer that falls
// behind misses whole rounds and is told how many, rather than accumulating a
// backlog that would push Audit and control traffic behind it.
type Subscription struct {
	hub    *Hub
	relay  *hostRelay
	id     uint64
	notify chan struct{}
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	latest  Frame
	has     bool
	err     error
	dropped atomic.Uint64
}

func (s *Subscription) put(frame Frame) {
	s.mu.Lock()
	if s.has {
		s.dropped.Add(1)
	}
	s.latest = frame
	s.has = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next returns the newest frame, waiting for one if none has arrived.
func (s *Subscription) Next(ctx context.Context) (Frame, error) {
	if ctx == nil {
		return Frame{}, errors.New("livematrix: Next context is required")
	}
	for {
		s.mu.Lock()
		if s.has {
			frame := s.latest
			s.has = false
			s.mu.Unlock()
			return frame, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-s.done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err != nil {
				return Frame{}, err
			}
			return Frame{}, io.EOF
		case <-s.notify:
		}
	}
}

// DroppedFrames counts rounds this viewer never saw.
func (s *Subscription) DroppedFrames() uint64 { return s.dropped.Load() }
func (s *Subscription) Done() <-chan struct{} { return s.done }

func (s *Subscription) Close() error {
	s.hub.unsubscribe(s)
	return nil
}

func (s *Subscription) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		close(s.done)
	})
}
