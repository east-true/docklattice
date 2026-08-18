package producttransport

import (
	"context"
	"time"
)

func runHeartbeatLoop(
	session ControlSession,
	interval time.Duration,
	timeout time.Duration,
	tickers TickerFactory,
	observer LivenessObserver,
) {
	ticker := tickers.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-session.Done():
			return
		case <-ticker.C():
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			_, err := session.Heartbeat(ctx)
			cancel()
			if observer != nil {
				observer(session.Info(), session.State(), err)
			}
		}
	}
}
