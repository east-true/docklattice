package producttransport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// StaleIncarnationError reports an attempted rollback of an Agent's durable
// incarnation watermark.
type StaleIncarnationError struct {
	AgentID      string
	Offered      uint64
	LastAccepted uint64
}

func (e *StaleIncarnationError) Error() string {
	return fmt.Sprintf("%v: agent %q offered %d after %d", ErrStaleIncarnation, e.AgentID, e.Offered, e.LastAccepted)
}

func (e *StaleIncarnationError) Unwrap() error { return ErrStaleIncarnation }

type registryRecord struct {
	lastIncarnation uint64
	loaded          bool
	active          ControlSession
}

// IncarnationWatermarkStore is implemented by the Server's durable Agent
// store (for example agents.last_incarnation). CompareAndSwap must be atomic
// across Server processes.
type IncarnationWatermarkStore interface {
	LoadIncarnation(context.Context, string) (uint64, error)
	CompareAndSwapIncarnation(context.Context, string, uint64, uint64) (bool, error)
}

// SessionRegistry enforces one active Reverse gRPC session per Agent and
// retains the highest accepted incarnation even while that Agent is offline.
type SessionRegistry struct {
	mu      sync.RWMutex
	records map[string]registryRecord
	store   IncarnationWatermarkStore
}

// NewSessionRegistry creates a process-local registry for unit-level use. A
// ServerAcceptor rejects it because it cannot prevent rollback after restart.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{records: make(map[string]registryRecord)}
}

func NewSessionRegistryWithStore(store IncarnationWatermarkStore) *SessionRegistry {
	return &SessionRegistry{records: make(map[string]registryRecord), store: store}
}

func (r *SessionRegistry) Register(session ControlSession) error {
	return r.RegisterContext(context.Background(), session)
}

func (r *SessionRegistry) RegisterContext(ctx context.Context, session ControlSession) error {
	if session == nil {
		return errors.New("session is required")
	}
	info := session.Info()
	if info.AgentID == "" || info.SessionID == "" || info.Incarnation == 0 {
		return errors.New("session Agent ID, session ID, and incarnation are required")
	}

	r.mu.Lock()
	record := r.records[info.AgentID]
	if r.store != nil {
		last, err := r.store.LoadIncarnation(ctx, info.AgentID)
		if err != nil {
			r.mu.Unlock()
			return fmt.Errorf("load incarnation watermark for agent %q: %w", info.AgentID, err)
		}
		if record.loaded && last < record.lastIncarnation {
			r.mu.Unlock()
			return fmt.Errorf("incarnation watermark for agent %q regressed from %d to %d", info.AgentID, record.lastIncarnation, last)
		}
		record.lastIncarnation = last
	}
	record.loaded = true
	if info.Incarnation < record.lastIncarnation {
		r.records[info.AgentID] = record
		r.mu.Unlock()
		return &StaleIncarnationError{
			AgentID: info.AgentID, Offered: info.Incarnation, LastAccepted: record.lastIncarnation,
		}
	}
	for r.store != nil && info.Incarnation > record.lastIncarnation {
		previousWatermark := record.lastIncarnation
		swapped, err := r.store.CompareAndSwapIncarnation(ctx, info.AgentID, record.lastIncarnation, info.Incarnation)
		if err != nil {
			r.mu.Unlock()
			return fmt.Errorf("advance incarnation watermark for agent %q: %w", info.AgentID, err)
		}
		if swapped {
			record.lastIncarnation = info.Incarnation
			break
		}
		last, err := r.store.LoadIncarnation(ctx, info.AgentID)
		if err != nil {
			r.mu.Unlock()
			return fmt.Errorf("reload incarnation watermark for agent %q: %w", info.AgentID, err)
		}
		record.lastIncarnation = last
		if last <= previousWatermark {
			r.mu.Unlock()
			return fmt.Errorf("incarnation watermark CAS for agent %q made no monotonic progress from %d", info.AgentID, previousWatermark)
		}
		if info.Incarnation < last {
			r.records[info.AgentID] = record
			r.mu.Unlock()
			return &StaleIncarnationError{AgentID: info.AgentID, Offered: info.Incarnation, LastAccepted: last}
		}
	}
	previous := record.active
	record.lastIncarnation = info.Incarnation
	record.active = session
	r.records[info.AgentID] = record
	r.mu.Unlock()

	if previous != nil && previous.Info().SessionID != info.SessionID {
		_ = previous.Close(ErrSessionReplaced)
	}
	return nil
}

// SessionClosed detaches only the named session. A close arriving from a
// replaced session therefore cannot mark its successor offline.
func (r *SessionRegistry) SessionClosed(agentID string, sessionID SessionID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[agentID]
	if !ok || record.active == nil || record.active.Info().SessionID != sessionID {
		return
	}
	record.active = nil
	r.records[agentID] = record
}

func (r *SessionRegistry) Current(agentID string) (ControlSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.records[agentID]
	return record.active, ok && record.active != nil
}

type RegistrySnapshot struct {
	AgentID         string
	LastIncarnation uint64
	ActiveSessionID SessionID
	State           State
	LastHeartbeat   time.Time
}

func (r *SessionRegistry) Snapshot(agentID string) (RegistrySnapshot, bool) {
	r.mu.RLock()
	record, ok := r.records[agentID]
	r.mu.RUnlock()
	if !ok {
		return RegistrySnapshot{}, false
	}
	snapshot := RegistrySnapshot{AgentID: agentID, LastIncarnation: record.lastIncarnation, State: StateClosed}
	if record.active != nil {
		snapshot.ActiveSessionID = record.active.Info().SessionID
		snapshot.State = record.active.State()
		snapshot.LastHeartbeat = record.active.LastHeartbeat()
	}
	return snapshot, true
}
