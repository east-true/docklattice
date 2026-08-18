// Package registrationhttp carries one-time Agent registration and credential
// renewal over an operator-configured HTTPS client/server boundary.
package registrationhttp

import (
	"errors"

	"github.com/east-true/dockpilot/internal/identity"
)

const (
	RegisterPath = "/api/v1/agent/register"
	RenewPath    = "/api/v1/agent/credential/renew"
	ActivatePath = "/api/v1/agent/credential/activate"
)

var (
	ErrRejected = errors.New("agent credential request rejected")
	ErrProtocol = errors.New("agent credential protocol error")
)

type ArchiveIdentity struct {
	ServerIdentityID string `json:"server_identity_id"`
	Generation       uint64 `json:"archive_generation"`
	AuditArchiveID   string `json:"audit_archive_id"`
}

type RegisterRequest struct {
	JoinToken         string               `json:"join_token"`
	AgentID           string               `json:"agent_id"`
	DisplayName       string               `json:"display_name"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
	ExpiredCredential *identity.Credential `json:"expired_credential,omitempty"`
}

type CredentialResponse struct {
	Credential identity.Credential `json:"credential"`
	Archive    ArchiveIdentity     `json:"archive"`
}

type RenewRequest struct {
	Current identity.Credential `json:"current_credential"`
}

type ActivateRequest struct {
	Previous identity.Credential `json:"previous_credential"`
	Active   identity.Credential `json:"active_credential"`
}

type ActivateResponse struct {
	Activated bool `json:"activated"`
}
