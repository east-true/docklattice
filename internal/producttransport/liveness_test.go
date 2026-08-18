package producttransport

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

type heartbeatLoopSession struct {
	*registryTestSession
	mu    sync.Mutex
	calls int
}

func (s *heartbeatLoopSession) Heartbeat(context.Context) (Heartbeat, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return Heartbeat{}, nil
}

func (s *heartbeatLoopSession) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestRunHeartbeatLoopUsesTickerAndStopsWithSession(t *testing.T) {
	ticker := newFakeTicker()
	session := &heartbeatLoopSession{registryTestSession: newRegistryTestSession(nil, "agent", 1, "session")}
	observed := make(chan State, 2)
	go runHeartbeatLoop(session, 30*time.Second, time.Second, TickerFactoryFunc(func(interval time.Duration) Ticker {
		if interval != 30*time.Second {
			t.Errorf("ticker interval = %s", interval)
		}
		return ticker
	}), func(_ SessionInfo, state State, err error) {
		if err != nil {
			t.Errorf("heartbeat observer error = %v", err)
		}
		observed <- state
	})

	for want := 1; want <= 2; want++ {
		ticker.ticks <- time.Now()
		select {
		case state := <-observed:
			if state != StateActive {
				t.Fatalf("observed state = %s", state)
			}
		case <-time.After(time.Second):
			t.Fatal("heartbeat was not observed")
		}
		if calls := session.callCount(); calls != want {
			t.Fatalf("heartbeat calls = %d, want %d", calls, want)
		}
	}
	_ = session.Close(nil)
	select {
	case <-ticker.done:
	case <-time.After(time.Second):
		t.Fatal("ticker did not stop with session")
	}
}
