// Package serverapi adapts the durable Server cache and current Agent sessions
// to webui.Backend without persisting live or secret-bearing responses.
package serverapi

import (
	"errors"
	"fmt"

	"github.com/east-true/dockpilot/internal/webui"
)

var (
	ErrAgentOffline = errors.New("serverapi: agent offline")
	ErrCorruptData  = errors.New("serverapi: corrupt data")
)

type OfflineError struct{ AgentID string }

func (e *OfflineError) Error() string {
	return fmt.Sprintf("%v: %q", ErrAgentOffline, e.AgentID)
}

func (e *OfflineError) Unwrap() []error {
	return []error{ErrAgentOffline, webui.ErrUnavailable}
}

type corruptDataError struct {
	boundary string
	cause    error
}

func (e *corruptDataError) Error() string {
	return fmt.Sprintf("%v at %s: %v", ErrCorruptData, e.boundary, e.cause)
}

func (e *corruptDataError) Unwrap() []error { return []error{ErrCorruptData, e.cause} }

type liveUnavailableError struct {
	agentID string
	action  string
	cause   error
}

func (e *liveUnavailableError) Error() string {
	// This error is rendered by webui as a client-visible 503. Keep transport
	// details in Unwrap for server-side diagnostics without reflecting them to
	// the browser, where they could contain endpoints or Agent-provided text.
	return fmt.Sprintf("serverapi: Agent %q %s unavailable", e.agentID, e.action)
}

func (e *liveUnavailableError) Unwrap() []error {
	return []error{webui.ErrUnavailable, e.cause}
}
