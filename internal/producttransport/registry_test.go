package producttransport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type registryTestSession struct {
	info      SessionInfo
	registry  *SessionRegistry
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.RWMutex
	err       error
}

type fakeWatermarkStore struct {
	mu     sync.Mutex
	values map[string]uint64
	cas    int
}

func newFakeWatermarkStore() *fakeWatermarkStore {
	return &fakeWatermarkStore{values: make(map[string]uint64)}
}

func (s *fakeWatermarkStore) LoadIncarnation(_ context.Context, agentID string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[agentID], nil
}

func (s *fakeWatermarkStore) CompareAndSwapIncarnation(_ context.Context, agentID string, old, next uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cas++
	if s.values[agentID] != old {
		return false, nil
	}
	s.values[agentID] = next
	return true, nil
}

func (s *fakeWatermarkStore) set(agentID string, value uint64) {
	s.mu.Lock()
	s.values[agentID] = value
	s.mu.Unlock()
}

func newRegistryTestSession(registry *SessionRegistry, agent string, incarnation uint64, id string) *registryTestSession {
	return &registryTestSession{
		info:     SessionInfo{AgentID: agent, Incarnation: incarnation, SessionID: SessionID(id)},
		registry: registry, done: make(chan struct{}),
	}
}

func (s *registryTestSession) Info() SessionInfo     { return s.info }
func (s *registryTestSession) Done() <-chan struct{} { return s.done }
func (s *registryTestSession) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}
func (s *registryTestSession) Close(cause error) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.err = cause
		s.mu.Unlock()
		close(s.done)
		if s.registry != nil {
			s.registry.SessionClosed(s.info.AgentID, s.info.SessionID)
		}
	})
	return nil
}
func (*registryTestSession) Heartbeat(context.Context) (Heartbeat, error) {
	return Heartbeat{}, nil
}
func (*registryTestSession) Query(context.Context, QueryRequest) (QueryResponse, error) {
	return QueryResponse{}, nil
}
func (*registryTestSession) StartOperation(context.Context, OperationRequest) (OperationResponse, error) {
	return OperationResponse{}, nil
}
func (*registryTestSession) OpenLogs(context.Context, LogRequest) (LogReceiveStream, error) {
	return nil, ErrHandlerUnavailable
}
func (*registryTestSession) OpenMetricsMatrix(context.Context, MetricsMatrixRequest) (MetricsMatrixReceiveStream, error) {
	return nil, errors.New("not implemented")
}
func (*registryTestSession) OpenStats(context.Context, StatsRequest) (StatsReceiveStream, error) {
	return nil, ErrHandlerUnavailable
}
func (s *registryTestSession) State() State {
	select {
	case <-s.done:
		return StateClosed
	default:
		return StateActive
	}
}
func (*registryTestSession) LastHeartbeat() time.Time { return time.Time{} }
func (*registryTestSession) Do(ctx context.Context, _ TrafficClass, work func(context.Context) error) error {
	return work(ctx)
}

func TestSessionRegistryReplacementAndStaleClose(t *testing.T) {
	registry := NewSessionRegistry()
	first := newRegistryTestSession(registry, "agent-1", 5, "session-1")
	second := newRegistryTestSession(registry, "agent-1", 5, "session-2")
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(second); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(first.Err(), ErrSessionReplaced) {
		t.Fatalf("replaced session error = %v", first.Err())
	}
	registry.SessionClosed("agent-1", "session-1")
	current, ok := registry.Current("agent-1")
	if !ok || current.Info().SessionID != "session-2" {
		t.Fatalf("current after stale close = %#v, %v", current, ok)
	}

	stale := newRegistryTestSession(registry, "agent-1", 4, "session-stale")
	err := registry.Register(stale)
	var incarnationError *StaleIncarnationError
	if !errors.Is(err, ErrStaleIncarnation) || !errors.As(err, &incarnationError) || incarnationError.LastAccepted != 5 {
		t.Fatalf("stale registration error = %#v", err)
	}
	if current, _ := registry.Current("agent-1"); current.Info().SessionID != "session-2" {
		t.Fatalf("stale registration replaced current with %#v", current.Info())
	}

	_ = second.Close(nil)
	if _, ok := registry.Current("agent-1"); ok {
		t.Fatal("closed current session remained active")
	}
	snapshot, ok := registry.Snapshot("agent-1")
	if !ok || snapshot.LastIncarnation != 5 || snapshot.State != StateClosed {
		t.Fatalf("offline watermark snapshot = %#v, %v", snapshot, ok)
	}
	if err := registry.Register(stale); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("offline incarnation rollback = %v", err)
	}
}

func TestSessionRegistryConcurrentMonotonicRegistration(t *testing.T) {
	registry := NewSessionRegistry()
	const sessions = 100
	var group sync.WaitGroup
	for incarnation := 1; incarnation <= sessions; incarnation++ {
		incarnation := incarnation
		group.Add(1)
		go func() {
			defer group.Done()
			session := newRegistryTestSession(registry, "agent", uint64(incarnation), fmt.Sprintf("session-%d", incarnation))
			err := registry.Register(session)
			if err != nil && !errors.Is(err, ErrStaleIncarnation) {
				t.Errorf("register incarnation %d: %v", incarnation, err)
			}
		}()
	}
	group.Wait()
	snapshot, ok := registry.Snapshot("agent")
	if !ok || snapshot.LastIncarnation != sessions {
		t.Fatalf("snapshot = %#v, %v", snapshot, ok)
	}
	current, ok := registry.Current("agent")
	if !ok || current.Info().Incarnation != sessions {
		t.Fatalf("current = %#v, %v", current, ok)
	}
}

func TestSessionRegistryDurableWatermarkSurvivesRestart(t *testing.T) {
	store := newFakeWatermarkStore()
	firstRegistry := NewSessionRegistryWithStore(store)
	if err := firstRegistry.Register(newRegistryTestSession(firstRegistry, "agent", 10, "first")); err != nil {
		t.Fatal(err)
	}

	secondRegistry := NewSessionRegistryWithStore(store)
	rollback := newRegistryTestSession(secondRegistry, "agent", 9, "rollback")
	if err := secondRegistry.Register(rollback); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("restart accepted incarnation rollback: %v", err)
	}
	equal := newRegistryTestSession(secondRegistry, "agent", 10, "equal")
	if err := secondRegistry.Register(equal); err != nil {
		t.Fatalf("same-incarnation reconnect failed: %v", err)
	}
	advanced := newRegistryTestSession(secondRegistry, "agent", 11, "advanced")
	if err := secondRegistry.Register(advanced); err != nil {
		t.Fatalf("incarnation advance failed: %v", err)
	}
	stored, _ := store.LoadIncarnation(context.Background(), "agent")
	if stored != 11 || store.cas != 2 {
		t.Fatalf("durable watermark = %d, CAS calls = %d", stored, store.cas)
	}
	store.set("agent", 20) // Simulate another Server accepting a newer incarnation.
	if err := secondRegistry.Register(newRegistryTestSession(secondRegistry, "agent", 12, "cross-server-stale")); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("registry ignored externally advanced watermark: %v", err)
	}
}
