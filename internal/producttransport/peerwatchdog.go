package producttransport

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
)

// DefaultPeerSilenceTimeout mirrors the Server's offline threshold. The Server
// heartbeats on P0 and declares an Agent offline once that window elapses; the
// Agent applies the same window to the Server so that both ends of one session
// agree on when it is dead.
const DefaultPeerSilenceTimeout = 90 * time.Second

// ErrPeerSilent reports that no Server call arrived within the silence window.
// Maintain treats the ended session like any other and reconnects with backoff.
var ErrPeerSilent = errors.New("producttransport: Server stopped calling within the silence window")

// peerWatchdog closes an Agent session whose Server has gone quiet.
//
// The Agent's side of a session is a gRPC server reading one connection it
// dialled itself. A Server that vanishes without closing that connection — a
// stopped container, a severed path, a host that drops packets rather than
// resetting them — leaves the read blocked with no error to observe, so the
// session never ends and the reconnect loop never runs. Observing inbound calls
// and applying the Server's own offline window makes that failure terminate the
// session instead.
type peerWatchdog struct {
	clock   Clock
	timeout time.Duration

	mu   sync.Mutex
	last time.Time
}

func newPeerWatchdog(clock Clock, timeout time.Duration) *peerWatchdog {
	if timeout <= 0 {
		timeout = DefaultPeerSilenceTimeout
	}
	w := &peerWatchdog{clock: clock, timeout: timeout}
	w.observe()
	return w
}

func (w *peerWatchdog) now() time.Time {
	if w.clock == nil {
		return time.Now().UTC()
	}
	return w.clock.Now().UTC()
}

func (w *peerWatchdog) observe() {
	w.mu.Lock()
	w.last = w.now()
	w.mu.Unlock()
}

func (w *peerWatchdog) silentFor(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Sub(w.last)
}

func (w *peerWatchdog) unaryInterceptor(
	ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	w.observe()
	return handler(ctx, request)
}

func (w *peerWatchdog) streamInterceptor(
	server any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler,
) error {
	w.observe()
	return handler(server, &watchedServerStream{ServerStream: stream, watchdog: w})
}

// watchedServerStream keeps a long-lived stream from ageing out while the
// Server is still sending on it.
type watchedServerStream struct {
	grpc.ServerStream
	watchdog *peerWatchdog
}

func (s *watchedServerStream) RecvMsg(message any) error {
	err := s.ServerStream.RecvMsg(message)
	if err == nil {
		s.watchdog.observe()
	}
	return err
}

func (w *peerWatchdog) run(session Session, tickers TickerFactory) {
	if session == nil {
		return
	}
	if tickers == nil {
		tickers = realTickerFactory{}
	}
	interval := w.timeout / 3
	if interval <= 0 {
		interval = w.timeout
	}
	ticker := tickers.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-session.Done():
			return
		case <-ticker.C():
			if w.silentFor(w.now()) >= w.timeout {
				_ = session.Close(ErrPeerSilent)
				return
			}
		}
	}
}
