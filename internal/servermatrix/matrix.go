// Package servermatrix fans one Agent's host metrics stream out to every
// browser watching that host.
//
// The Agent already assembles whole frames and already keeps only the newest
// one per consumer. This package does not repeat that work and does not
// resample anything: it opens exactly one stream per host no matter how many
// viewers arrive, holds the newest frame per viewer, and ends the stream when
// the last viewer leaves.
//
// The frame stays the unit of loss here for the same reason it is on the Agent.
// A slow browser misses whole rounds of everything and is told how many, rather
// than accumulating a backlog that would push Audit and control traffic behind
// it.
package servermatrix

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/east-true/docklattice/internal/producttransport"
)

var ErrClosed = errors.New("server metrics matrix is closed")

// FrameStream is one open Agent metrics stream. It is the transport as this
// package needs to see it, which is nothing more than the two calls below.
type FrameStream interface {
	Recv(context.Context) (producttransport.MetricsMatrixFrame, error)
	Close() error
}

// Sessions opens a host's frame stream. The implementation owns the questions
// this package deliberately does not ask: whether the Agent is connected,
// whether it reports the metrics capability, and what to say when it does not.
// A host without the capability fails here with a reason rather than producing
// an empty stream that nobody can explain.
type Sessions interface {
	// Open is given the relay's own context, which lives until the last viewer
	// leaves, so the returned stream may hold it. Any bounded probe the
	// implementation performs while opening is its own to bound.
	Open(ctx context.Context, agentID string) (FrameStream, error)
}

// ContainerContext is discovery's account of one container: which Compose
// project and service it belongs to, and the image it was started from.
//
// It is context, not authority. The Docker Engine decides what is running; this
// only says what DockLattice knows about it, and knowing nothing is an ordinary
// answer - a container started by hand is not part of any project and never
// will be.
type ContainerContext struct {
	ProjectUID  string
	ProjectName string
	Service     string
	Image       string
}

// ContextSource answers what discovery knows about the containers on one host,
// keyed by container ID.
//
// The implementation owns where the answer comes from and bounds its own call.
// It is asked on the relay's context, off the frame path, because a frame must
// never wait for discovery: metrics are the live thing here and project names
// are not.
type ContextSource interface {
	ContainerContext(ctx context.Context, agentID string) (map[string]ContainerContext, error)
}

// Filesystem is capacity for one path DockLattice writes to, carried through from
// the Agent. Unavailable is a fact about that path, not about the host.
type Filesystem struct {
	Path        string
	TotalBytes  uint64
	FreeBytes   uint64
	Unavailable bool
	Reason      string
}

// HostRow is the Docker workload an Agent manages against the capacity its
// Engine reports. It is not host OS metrics and must never be labelled as such:
// an Agent runs in a container, and what it can see of the host from there is
// the Engine's account of itself, not /proc.
type HostRow struct {
	CPUCapacity       uint32
	MemoryCapacity    uint64
	ContainersRunning uint32
	ContainersTotal   uint32
	Filesystems       []Filesystem
	// Totals is every container in this frame added up, by the same rules as
	// every other aggregate row. Its memory limit is the sum of the containers'
	// limits and is unbounded as soon as one of them is unlimited, which is a
	// different quantity from MemoryCapacity above: what the containers are
	// allowed and what the machine has are not the same question, and a view
	// must not let them be read as one.
	Totals Aggregate
}

// ContainerRow is one container in a view. Pending carries the Agent's meaning
// unchanged: the container is a member of the frame but its first sample has
// not arrived, which is a different state from being gone and is shown as one.
type ContainerRow struct {
	ContainerID string
	Pending     bool
	Sample      producttransport.StatsSample
	ContainerContext
	// Unmapped says this container belongs to no project. The row is still
	// here, with its metrics, because the Engine is the authority for what is
	// running and a failed metadata join must not erase a running container.
	// Hiding it would make the matrix quietly wrong exactly when something has
	// just been deployed.
	Unmapped bool
	// MemoryLimitUnbounded and the two percent fields are derived once here,
	// against this frame's own Engine capacity; see markMemoryLimit for why the
	// reported limit cannot simply be read as one.
	MemoryLimitUnbounded bool
	MemoryPercent        float64
	MemoryPercentKnown   bool
}

// View is one host at one instant, as a browser should see it.
//
// The two dropped counts are separate because they are different failures with
// different fixes. AgentDropped is what the Agent discarded because this Server
// was slow to read the stream; ViewerDropped is what this Server discarded
// because this browser was slow to read its subscription. Adding them together
// would hide which side is behind.
type View struct {
	AgentID    string
	ObservedAt time.Time
	Host       HostRow
	// Projects is the whole host as a tree: project, then service, then
	// container. Containers appear once, under the service they belong to, so
	// the view carries each row's numbers exactly once no matter how many
	// levels display them.
	Projects         []ProjectRow
	AgentDropped     uint64
	ViewerDropped    uint64
	MembershipStale  bool
	MembershipReason string
	WorkloadStale    bool
	WorkloadReason   string
	// ContextStale says the project and service names on these rows are the
	// last ones discovery could confirm, or none at all, with ContextReason
	// saying why. It matters because unmapped has two causes that look
	// identical on a row: a container nothing manages, and a container we could
	// not ask about. Only this tells them apart.
	ContextStale  bool
	ContextReason string
}

type Config struct {
	Sessions Sessions
	Context  ContextSource
	// ContextRefresh is how often discovery is asked again for a host that is
	// being watched. Project membership changes when something is deployed, not
	// every frame, so this is far slower than the frame cadence.
	ContextRefresh time.Duration
	// ContextRetryInterval is the floor between refreshes triggered by a
	// container ID discovery has never answered for. It exists so a newly
	// deployed container gets its project name in seconds rather than at the
	// next periodic refresh - and it is a floor rather than a rule because a
	// container nothing manages would otherwise ask forever. Such an ID is
	// asked about once and then left to the periodic cadence.
	ContextRetryInterval time.Duration
	Clock                Clock
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Hub owns one relay per host. Every viewer of a host shares it: the Agent
// stream exists exactly once, from the first viewer until the last one leaves.
type Hub struct {
	config Config

	mu         sync.Mutex
	closed     bool
	nextViewer uint64
	relays     map[string]*hostRelay
}

type hostRelay struct {
	agentID string
	ctx     context.Context
	cancel  context.CancelFunc

	// Everything below is guarded by the Hub's lock. A relay has no lock of
	// its own: one lock over the whole hub is what makes "is this relay still
	// the one registered for this host" answerable at all, and every operation
	// under it is a map write or a pointer copy rather than anything slow.
	//
	// ready is closed once the open attempt has settled, successfully or not.
	// Viewers that arrive while it is in flight wait on it rather than starting
	// a second stream to the same host.
	ready   chan struct{}
	stream  FrameStream
	openErr error
	endErr  error
	viewers map[uint64]*Subscription

	// contextByID is replaced wholesale on every successful refresh and never
	// mutated after publication, so a frame may carry the reference rather than
	// a copy of it.
	contextByID map[string]ContainerContext
	haveContext bool
	// contextTried is the last attempt, successful or not, and is what the
	// refresh intervals are measured from: a discovery that keeps failing must
	// not be asked once per frame.
	contextTried  time.Time
	contextReason string
	refreshing    bool
	lastFrameIDs  map[string]struct{}
	// unmatchedIDs are the containers that were on the host and still absent
	// from discovery when the last refresh finished. Asking again about them
	// would produce the same answer, so they no longer trigger early refreshes.
	unmatchedIDs map[string]struct{}
}

func New(config Config) (*Hub, error) {
	if config.Sessions == nil {
		return nil, errors.New("servermatrix: session source is required")
	}
	if config.Context == nil {
		return nil, errors.New("servermatrix: container context source is required")
	}
	if config.ContextRefresh <= 0 {
		config.ContextRefresh = time.Minute
	}
	if config.ContextRetryInterval <= 0 {
		config.ContextRetryInterval = 10 * time.Second
	}
	if config.ContextRetryInterval > config.ContextRefresh {
		config.ContextRetryInterval = config.ContextRefresh
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	return &Hub{config: config, relays: make(map[string]*hostRelay)}, nil
}

// Subscribe attaches a viewer to a host. The first viewer opens the Agent
// stream and waits for that to succeed or fail, so a host that cannot be
// watched says so at subscribe time instead of delivering silence.
func (h *Hub) Subscribe(ctx context.Context, agentID string) (*Subscription, error) {
	if ctx == nil {
		return nil, errors.New("servermatrix: viewer context is required")
	}
	if agentID == "" {
		return nil, errors.New("servermatrix: Agent ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	relay, existing := h.relays[agentID]
	if !existing {
		relayCtx, cancel := context.WithCancel(context.Background())
		relay = &hostRelay{
			agentID: agentID, ctx: relayCtx, cancel: cancel,
			ready: make(chan struct{}), viewers: make(map[uint64]*Subscription),
		}
		h.relays[agentID] = relay
	}
	h.nextViewer++
	viewer := &Subscription{
		hub: h, relay: relay, id: h.nextViewer,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	relay.viewers[viewer.id] = viewer
	h.mu.Unlock()

	if !existing {
		h.open(relay)
	}
	select {
	case <-relay.ready:
	case <-ctx.Done():
		// Leaving without waiting would strand the relay this viewer may be the
		// only reason for, so the departure goes through the normal path.
		_ = viewer.Close()
		return nil, ctx.Err()
	}
	h.mu.Lock()
	openErr := relay.openErr
	h.mu.Unlock()
	if openErr != nil {
		_ = viewer.Close()
		return nil, openErr
	}
	return viewer, nil
}

// open performs the one stream open for a relay. It holds no Hub lock while
// calling the transport: opening talks to an Agent, and a slow or unreachable
// host must not stop every other host's viewers from arriving and leaving.
func (h *Hub) open(relay *hostRelay) {
	stream, err := h.config.Sessions.Open(relay.ctx, relay.agentID)
	h.mu.Lock()
	relay.stream, relay.openErr = stream, err
	h.mu.Unlock()
	close(relay.ready)
	if err != nil {
		h.stopRelay(relay, err)
		return
	}
	// One context read before the first frame, so an opening view carries
	// project names rather than showing every container as unmapped for a
	// round. Its failure is not the stream's failure: metrics without project
	// names are still metrics, and the view says why the names are missing.
	if h.beginRefresh(relay) {
		h.refreshContext(relay)
	}
	go h.run(relay)
}

// run reads frames for as long as the Agent sends them. There is no ticker
// here: the Agent's frame cadence is the cadence, and inventing a second one on
// the Server would either resample a frame nobody re-measured or drop one
// nobody asked to lose.
func (h *Hub) run(relay *hostRelay) {
	var endErr error
	defer func() { h.stopRelay(relay, endErr) }()
	h.mu.Lock()
	stream := relay.stream
	h.mu.Unlock()
	for {
		frame, err := stream.Recv(relay.ctx)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				endErr = err
			}
			return
		}
		h.publish(relay, frame)
	}
}

// publish turns one Agent frame into one view and hands it to every viewer.
func (h *Hub) publish(relay *hostRelay, frame producttransport.MetricsMatrixFrame) {
	h.mu.Lock()
	if h.relays[relay.agentID] != relay {
		h.mu.Unlock()
		return
	}
	viewers := make([]*Subscription, 0, len(relay.viewers))
	for _, viewer := range relay.viewers {
		viewers = append(viewers, viewer)
	}
	unknown := h.noteFrameMembership(relay, frame)
	mapping, haveContext, contextReason := relay.contextByID, relay.haveContext, relay.contextReason
	h.mu.Unlock()

	view := h.assemble(relay, frame, mapping, haveContext, contextReason)
	for _, viewer := range viewers {
		viewer.put(view)
	}
	h.maybeRefreshContext(relay, unknown)
}

// noteFrameMembership records what this host is running, so a refresh that
// finishes later can tell which containers discovery genuinely does not manage
// from those that simply had not been asked about yet. It also answers whether
// this frame holds a container worth asking about now.
func (h *Hub) noteFrameMembership(relay *hostRelay, frame producttransport.MetricsMatrixFrame) bool {
	ids := make(map[string]struct{}, len(frame.Containers)+len(frame.PendingContainerIDs))
	for _, sample := range frame.Containers {
		ids[sample.ContainerID] = struct{}{}
	}
	for _, id := range frame.PendingContainerIDs {
		ids[id] = struct{}{}
	}
	relay.lastFrameIDs = ids
	for id := range ids {
		if _, known := relay.contextByID[id]; known {
			continue
		}
		if _, asked := relay.unmatchedIDs[id]; asked {
			continue
		}
		return true
	}
	return false
}

// maybeRefreshContext decides whether to ask discovery again. It never waits
// for the answer, so no frame is delayed by it, and only one refresh per host
// is ever in flight.
func (h *Hub) maybeRefreshContext(relay *hostRelay, unknown bool) {
	interval := h.config.ContextRefresh
	if unknown {
		interval = h.config.ContextRetryInterval
	}
	h.mu.Lock()
	due := h.relays[relay.agentID] == relay && !relay.refreshing &&
		(relay.contextTried.IsZero() || h.config.Clock.Now().Sub(relay.contextTried) >= interval)
	if due {
		relay.refreshing = true
	}
	h.mu.Unlock()
	if due {
		go h.refreshContext(relay)
	}
}

// beginRefresh claims the single in-flight refresh slot for a relay.
func (h *Hub) beginRefresh(relay *hostRelay) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.relays[relay.agentID] != relay || relay.refreshing {
		return false
	}
	relay.refreshing = true
	return true
}

func (h *Hub) refreshContext(relay *hostRelay) {
	mapping, err := h.config.Context.ContainerContext(relay.ctx, relay.agentID)
	h.mu.Lock()
	defer h.mu.Unlock()
	relay.refreshing = false
	if h.relays[relay.agentID] != relay {
		return
	}
	relay.contextTried = h.config.Clock.Now()
	if err != nil {
		// The previous mapping stays. Dropping it would turn one failed
		// discovery call into every container losing its project name, which
		// is a worse answer than a slightly old one that says it is old.
		relay.contextReason = boundedReason(err)
		return
	}
	relay.contextByID, relay.haveContext, relay.contextReason = mapping, true, ""
	unmatched := make(map[string]struct{})
	for id := range relay.lastFrameIDs {
		if _, known := mapping[id]; !known {
			unmatched[id] = struct{}{}
		}
	}
	relay.unmatchedIDs = unmatched
}

// boundedReason keeps a discovery error short enough to travel in every view.
// It is a reason for an operator, not a payload.
func boundedReason(err error) string {
	const limit = 200
	message := err.Error()
	if message == "" {
		message = "container context lookup failed"
	}
	if len(message) > limit {
		message = message[:limit]
	}
	return message
}

func (h *Hub) assemble(
	relay *hostRelay, frame producttransport.MetricsMatrixFrame,
	mapping map[string]ContainerContext, haveContext bool, contextReason string,
) View {
	containers := make([]ContainerRow, 0, len(frame.Containers)+len(frame.PendingContainerIDs))
	for _, sample := range frame.Containers {
		containers = append(containers, containerRow(sample.ContainerID, false, sample, mapping))
	}
	for _, id := range frame.PendingContainerIDs {
		containers = append(containers, containerRow(id, true, producttransport.StatsSample{}, mapping))
	}
	sortContainerRows(containers)
	for index := range containers {
		markMemoryLimit(&containers[index], frame.Workload.MemoryCapacity)
	}

	filesystems := make([]Filesystem, 0, len(frame.Workload.Filesystems))
	for _, filesystem := range frame.Workload.Filesystems {
		filesystems = append(filesystems, Filesystem{
			Path: filesystem.Path, TotalBytes: filesystem.TotalBytes, FreeBytes: filesystem.FreeBytes,
			Unavailable: filesystem.Unavailable, Reason: filesystem.Reason,
		})
	}
	return View{
		AgentID:    relay.agentID,
		ObservedAt: frame.ObservedAt,
		Host: HostRow{
			CPUCapacity: frame.Workload.CPUCapacity, MemoryCapacity: frame.Workload.MemoryCapacity,
			ContainersRunning: frame.Workload.ContainersRunning, ContainersTotal: frame.Workload.ContainersTotal,
			Filesystems: filesystems, Totals: aggregate(containers),
		},
		Projects:        groupProjects(containers),
		AgentDropped:    frame.DroppedFrames,
		MembershipStale: frame.MembershipStale, MembershipReason: frame.MembershipReason,
		WorkloadStale: frame.WorkloadStale, WorkloadReason: frame.WorkloadReason,
		ContextStale: !haveContext || contextReason != "", ContextReason: contextReason,
	}
}

func containerRow(
	id string, pending bool, sample producttransport.StatsSample, mapping map[string]ContainerContext,
) ContainerRow {
	row := ContainerRow{ContainerID: id, Pending: pending, Sample: sample, ContainerContext: mapping[id]}
	// Unmapped asks whether this container belongs to a project, not whether
	// the lookup returned anything. Knowing a container's image while knowing
	// no project for it is the ordinary state of a container somebody started
	// by hand, and it is exactly as unmapped as one nothing is known about.
	row.Unmapped = row.ProjectUID == "" && row.ProjectName == ""
	return row
}

// sortContainerRows gives every view one order, by container ID. The Agent
// sorts its rows too, but the pending IDs arrive on their own list and would
// otherwise land after every sampled row - a container would appear to jump
// down the table on the frame it went quiet and back up on the next one.
func sortContainerRows(rows []ContainerRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ContainerID < rows[j].ContainerID })
}

func (h *Hub) unsubscribe(viewer *Subscription) {
	relay := viewer.relay
	if relay == nil {
		viewer.finish(nil)
		return
	}
	h.mu.Lock()
	if h.relays[relay.agentID] != relay {
		// The relay has already been torn down, and stopRelay finished every
		// viewer it held. Nothing here owns anything any more.
		h.mu.Unlock()
		viewer.finish(nil)
		return
	}
	delete(relay.viewers, viewer.id)
	last := len(relay.viewers) == 0
	if last {
		delete(h.relays, relay.agentID)
	}
	h.mu.Unlock()

	viewer.finish(nil)
	if last {
		// Cancelling ends the Recv loop, whose deferred stopRelay closes the
		// Agent stream. Nothing is collected on the Agent once that happens.
		relay.cancel()
	}
}

// stopRelay unwinds a relay exactly once from whichever side ended it.
func (h *Hub) stopRelay(relay *hostRelay, cause error) {
	h.mu.Lock()
	if h.relays[relay.agentID] == relay {
		delete(h.relays, relay.agentID)
	}
	if relay.endErr == nil {
		relay.endErr = cause
	}
	endErr := relay.endErr
	stream := relay.stream
	relay.stream = nil
	viewers := make([]*Subscription, 0, len(relay.viewers))
	for _, viewer := range relay.viewers {
		viewers = append(viewers, viewer)
	}
	relay.viewers = make(map[uint64]*Subscription)
	h.mu.Unlock()

	relay.cancel()
	if stream != nil {
		_ = stream.Close()
	}
	for _, viewer := range viewers {
		if endErr != nil {
			viewer.finish(endErr)
			continue
		}
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
	relays := make([]*hostRelay, 0, len(h.relays))
	for _, relay := range h.relays {
		relays = append(relays, relay)
	}
	h.relays = make(map[string]*hostRelay)
	h.mu.Unlock()
	for _, relay := range relays {
		relay.cancel()
	}
	return nil
}

// Subscription is one browser's views. It holds one view, for the reason the
// package comment gives: a viewer that falls behind misses whole rounds and is
// told how many.
type Subscription struct {
	hub    *Hub
	relay  *hostRelay
	id     uint64
	notify chan struct{}
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	latest  View
	has     bool
	err     error
	dropped atomic.Uint64
}

func (s *Subscription) put(view View) {
	s.mu.Lock()
	if s.has {
		s.dropped.Add(1)
	}
	view.ViewerDropped = s.dropped.Load()
	s.latest = view
	s.has = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next returns the newest view, waiting for one if none has arrived.
func (s *Subscription) Next(ctx context.Context) (View, error) {
	if ctx == nil {
		return View{}, errors.New("servermatrix: Next context is required")
	}
	for {
		s.mu.Lock()
		if s.has {
			view := s.latest
			s.has = false
			s.mu.Unlock()
			return view, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return View{}, ctx.Err()
		case <-s.done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err != nil {
				return View{}, err
			}
			return View{}, io.EOF
		case <-s.notify:
		}
	}
}

// DroppedViews counts rounds this viewer never saw.
func (s *Subscription) DroppedViews() uint64  { return s.dropped.Load() }
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
