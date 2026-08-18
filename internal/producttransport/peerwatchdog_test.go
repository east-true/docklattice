package producttransport

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type watchdogSession struct {
	mu    sync.Mutex
	done  chan struct{}
	cause error
	once  sync.Once
}

func newWatchdogSession() *watchdogSession {
	return &watchdogSession{done: make(chan struct{})}
}

func (s *watchdogSession) Info() SessionInfo     { return SessionInfo{AgentID: "agent-a"} }
func (s *watchdogSession) Done() <-chan struct{} { return s.done }

func (s *watchdogSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

func (s *watchdogSession) Close(cause error) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.cause = cause
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppingClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestPeerWatchdogClosesTheSessionAfterServerSilence(t *testing.T) {
	clock := &steppingClock{now: time.Unix(1000, 0).UTC()}
	watchdog := newPeerWatchdog(clock, 90*time.Second)
	session := newWatchdogSession()
	ticker := newFakeTicker()
	go watchdog.run(session, TickerFactoryFunc(func(time.Duration) Ticker { return ticker }))

	// Still inside the window: the session must survive.
	clock.advance(89 * time.Second)
	ticker.ticks <- clock.Now()
	select {
	case <-session.Done():
		t.Fatal("the session was closed before the silence window elapsed")
	case <-time.After(50 * time.Millisecond):
	}

	clock.advance(2 * time.Second)
	ticker.ticks <- clock.Now()
	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a silent Server did not end the session")
	}
	if !errors.Is(session.Err(), ErrPeerSilent) {
		t.Fatalf("close cause = %v", session.Err())
	}
}

func TestPeerWatchdogKeepsTheSessionWhileTheServerCalls(t *testing.T) {
	clock := &steppingClock{now: time.Unix(1000, 0).UTC()}
	watchdog := newPeerWatchdog(clock, 90*time.Second)
	session := newWatchdogSession()
	ticker := newFakeTicker()
	go watchdog.run(session, TickerFactoryFunc(func(time.Duration) Ticker { return ticker }))

	for i := 0; i < 5; i++ {
		clock.advance(60 * time.Second)
		watchdog.observe()
		ticker.ticks <- clock.Now()
	}
	select {
	case <-session.Done():
		t.Fatal("an actively called session was closed")
	case <-time.After(50 * time.Millisecond):
	}
	session.Close(nil)
}

func TestPeerWatchdogDefaultsToTheServerOfflineWindow(t *testing.T) {
	watchdog := newPeerWatchdog(nil, 0)
	if watchdog.timeout != DefaultPeerSilenceTimeout {
		t.Fatalf("default timeout = %v", watchdog.timeout)
	}
	if DefaultPeerSilenceTimeout != 90*time.Second {
		t.Fatalf("the Agent silence window must match the Server offline threshold, got %v", DefaultPeerSilenceTimeout)
	}
}
