package producttransport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"
)

type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

type timerSleeper struct{}

func (timerSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Random interface {
	Float64() float64
}

type ReconnectPolicy struct {
	Initial    time.Duration
	Maximum    time.Duration
	Multiplier float64
	Jitter     float64
}

func DefaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{Initial: time.Second, Maximum: time.Minute, Multiplier: 2, Jitter: 0.2}
}

func (p ReconnectPolicy) validate() error {
	if p.Initial <= 0 || p.Maximum < p.Initial || p.Multiplier < 1 || p.Jitter < 0 || p.Jitter >= 1 {
		return fmt.Errorf("invalid reconnect policy")
	}
	return nil
}

func (p ReconnectPolicy) delay(attempt int, random Random) time.Duration {
	base := float64(p.Initial) * math.Pow(p.Multiplier, float64(attempt))
	if base > float64(p.Maximum) {
		base = float64(p.Maximum)
	}
	factor := 1.0
	if p.Jitter > 0 {
		factor += p.Jitter * (2*random.Float64() - 1)
	}
	delay := time.Duration(base * factor)
	if delay > p.Maximum {
		return p.Maximum
	}
	return delay
}

type ConnectFunc func(context.Context) (Session, error)

// Maintain reconnects with bounded exponential backoff until ctx is canceled.
// Every ended session is followed by backoff; successful connection prevents
// a tight reconnect loop but resets subsequent failure growth.
func Maintain(ctx context.Context, connect ConnectFunc, policy ReconnectPolicy, sleeper Sleeper, random Random, onConnect func(Session)) error {
	if connect == nil {
		return errors.New("connect function is required")
	}
	if err := policy.validate(); err != nil {
		return err
	}
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	if random == nil {
		random = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		session, err := connect(ctx)
		if err == nil && session == nil {
			err = errors.New("connect returned a nil session")
		}
		if err == nil {
			attempt = 0
			if onConnect != nil {
				onConnect(session)
			}
			select {
			case <-ctx.Done():
				_ = session.Close(ctx.Err())
				return nil
			case <-session.Done():
			}
		}
		delay := policy.delay(attempt, random)
		attempt++
		if err := sleeper.Sleep(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
