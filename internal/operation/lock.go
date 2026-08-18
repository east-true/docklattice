package operation

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type heldLock struct {
	owner    string
	released chan struct{}
}

// LockManager owns Agent-authoritative project-exclusive locks.
type LockManager struct {
	mu   sync.Mutex
	wait time.Duration
	held map[string]*heldLock
}

func NewLockManager(wait time.Duration) *LockManager {
	return &LockManager{wait: wait, held: make(map[string]*heldLock)}
}

// Lease represents one project lock acquisition.
type Lease struct {
	manager *LockManager
	project string
	owner   string
	once    sync.Once
}

// Acquire waits up to the configured bound, then returns PROJECT_BUSY. It does
// not create an unbounded operation queue.
func (m *LockManager) Acquire(ctx context.Context, project, operationID string) (*Lease, error) {
	if project == "" || operationID == "" {
		return nil, &Error{Code: CodeInvalidSpec, Message: "project and operation ID are required for locking"}
	}
	var deadline *time.Timer
	var timeout <-chan time.Time
	if m.wait > 0 {
		deadline = time.NewTimer(m.wait)
		timeout = deadline.C
		defer deadline.Stop()
	}

	for {
		m.mu.Lock()
		current := m.held[project]
		if current == nil {
			m.held[project] = &heldLock{owner: operationID, released: make(chan struct{})}
			m.mu.Unlock()
			return &Lease{manager: m, project: project, owner: operationID}, nil
		}
		released := current.released
		owner := current.owner
		m.mu.Unlock()

		if m.wait <= 0 {
			return nil, projectBusy(project, owner)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, projectBusy(project, owner)
		case <-released:
		}
	}
}

func projectBusy(project, owner string) error {
	return &Error{Code: CodeProjectBusy, Message: fmt.Sprintf("project %q is locked by operation %q", project, owner)}
}

func (l *Lease) Release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		current := m.held[l.project]
		if current != nil && current.owner == l.owner {
			delete(m.held, l.project)
			close(current.released)
		}
		m.mu.Unlock()
	})
}
