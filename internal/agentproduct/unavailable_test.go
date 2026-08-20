package agentproduct

import (
	"context"
	"errors"
	"testing"

	"github.com/east-true/dockpilot/internal/producttransport"
)

// A Handler whose stream handlers were never installed answers, rather than
// dying. New cannot produce one - the constructor test below holds that line -
// but the process boundary must not depend on an invariant held one layer away.
//
// *Handler satisfies every stream handler interface unconditionally, so the
// transport's own "handler is not configured" check can never fire for this
// type: the assertion always succeeds and whatever is behind the field is
// called. Without these guards, a call arriving anyway - an older Server, a
// replayed request, a future build that makes one of these optional - would
// take the Agent process down.
func TestUnconfiguredStreamHandlersFailClosedInsteadOfPanicking(t *testing.T) {
	handler := &Handler{}
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"metrics matrix": func() error {
			return handler.StreamMetricsMatrix(ctx, producttransport.SessionInfo{}, producttransport.MetricsMatrixRequest{}, nil)
		},
		"stats": func() error {
			return handler.StreamStats(ctx, producttransport.SessionInfo{}, producttransport.StatsRequest{}, nil)
		},
		"logs": func() error {
			return handler.StreamLogs(ctx, producttransport.SessionInfo{}, producttransport.LogRequest{}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A panic here fails the test rather than the process, which is the
			// whole difference this guard makes.
			err := call()
			if !errors.Is(err, producttransport.ErrHandlerUnavailable) {
				t.Fatalf("%s answered %v, want the existing unavailable error", name, err)
			}
		})
	}
}

// The bridge one layer down answers the same way when it was built without a
// hub, so neither layer relies on the other having checked.
func TestMatrixBridgeWithoutAHubIsUnavailable(t *testing.T) {
	bridge := producttransport.LiveMatrixHandler{}
	err := bridge.StreamMetricsMatrix(context.Background(), producttransport.SessionInfo{}, producttransport.MetricsMatrixRequest{}, nil)
	if !errors.Is(err, producttransport.ErrHandlerUnavailable) {
		t.Fatalf("a matrix bridge with no hub answered %v", err)
	}
}

// New refuses a configuration it cannot serve rather than building a Handler
// that reports a capability it cannot honour. This is what makes the guards
// above unreachable in a running Agent, and it is the half that must not
// silently relax.
func TestNewRefusesAMatrixItCannotServe(t *testing.T) {
	base, _, _ := validConfig(t)
	base.MatrixDocker = nil
	if _, err := New(base); err == nil {
		t.Fatal("New built a Handler with no Docker behind the metrics matrix")
	}

	base, _, _ = validConfig(t)
	base.MatrixFrameInterval = 0
	if _, err := New(base); err == nil {
		t.Fatal("New built a Handler with no frame cadence")
	}
}
