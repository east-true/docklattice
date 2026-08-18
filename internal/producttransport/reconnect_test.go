package producttransport

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type recordingSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *recordingSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.delays = append(s.delays, duration)
	s.mu.Unlock()
	return nil
}

type fixedRandom float64

func (r fixedRandom) Float64() float64 { return float64(r) }

type fakeSession struct{ sessionCore }

func newFakeSession() *fakeSession {
	return &fakeSession{sessionCore: newSessionCore(SessionInfo{SessionID: "session"})}
}

func (s *fakeSession) Close(err error) error {
	s.finish(err)
	return nil
}

func TestMaintainReconnectsWithBackoffAndStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	sleeper := &recordingSleeper{}
	err := Maintain(ctx, func(context.Context) (Session, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("offline")
		}
		return newFakeSession(), nil
	}, ReconnectPolicy{Initial: time.Second, Maximum: 10 * time.Second, Multiplier: 2, Jitter: 0}, sleeper, fixedRandom(0.5), func(Session) {
		cancel()
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("connect attempts = %d", attempts)
	}
	sleeper.mu.Lock()
	delays := append([]time.Duration(nil), sleeper.delays...)
	sleeper.mu.Unlock()
	if !reflect.DeepEqual(delays, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("backoff delays = %v", delays)
	}
}

func TestReconnectDelayJitterAndCap(t *testing.T) {
	policy := ReconnectPolicy{Initial: time.Second, Maximum: 5 * time.Second, Multiplier: 2, Jitter: 0.2}
	if got := policy.delay(0, fixedRandom(0)); got != 800*time.Millisecond {
		t.Fatalf("low jitter delay = %s", got)
	}
	if got := policy.delay(20, fixedRandom(1)); got != 5*time.Second {
		t.Fatalf("capped delay = %s", got)
	}
}
