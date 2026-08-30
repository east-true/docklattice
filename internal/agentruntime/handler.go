package agentruntime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/east-true/docklattice/internal/auditsync"
	"github.com/east-true/docklattice/internal/producttransport"
)

// heartbeatHandler intentionally implements only AgentHandler. Query,
// operation, log, and stats RPCs therefore receive the transport's typed
// unimplemented response until their real handlers are wired.
type heartbeatHandler struct {
	mu               sync.RWMutex
	agentID          string
	incarnation      uint64
	credentialID     string
	serverIdentityID string
	capability       producttransport.Capability
}

type auditHeartbeatHandler struct {
	*heartbeatHandler
	audit *auditsync.Agent
}

func (h *auditHeartbeatHandler) SyncAudit(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditSyncStream) error {
	return h.audit.SyncAudit(ctx, info, stream)
}

func (h *heartbeatHandler) Heartbeat(_ context.Context, info producttransport.SessionInfo, _ time.Time) (producttransport.Capability, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if info.AgentID != h.agentID || info.Incarnation != h.incarnation ||
		info.CredentialID != h.credentialID || info.ServerIdentityID != h.serverIdentityID {
		return producttransport.Capability{}, fmt.Errorf("agentruntime: heartbeat session identity mismatch")
	}
	return h.capability, nil
}

func (h *heartbeatHandler) setCredentialIdentity(credentialID, serverIdentityID string) {
	h.mu.Lock()
	h.credentialID, h.serverIdentityID = credentialID, serverIdentityID
	h.mu.Unlock()
}

func (h *heartbeatHandler) setCapability(capability producttransport.Capability) {
	h.mu.Lock()
	h.capability = capability
	h.mu.Unlock()
}
