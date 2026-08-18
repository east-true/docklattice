package experiment

import (
	"context"
	"testing"

	"github.com/east-true/dockpilot/internal/transport"
)

func TestExpectedEndIncludesTransportDeadline(t *testing.T) {
	if !expectedEnd(transport.Wrap(transport.CodeDeadlineExceeded, context.DeadlineExceeded, "call ended")) {
		t.Fatal("transport deadline at scenario shutdown must be treated as an expected end")
	}
}
