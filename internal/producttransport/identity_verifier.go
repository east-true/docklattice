package producttransport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/east-true/dockpilot/internal/identity"
)

// IdentityVerifier adapts the Server Identity State credential implementation
// to the transport's opaque credential boundary.
type IdentityVerifier struct {
	Manager *identity.Manager
}

func (v IdentityVerifier) VerifyCredential(_ context.Context, payload []byte, now time.Time) (CredentialIdentity, error) {
	if v.Manager == nil {
		return CredentialIdentity{}, errors.New("identity manager is required")
	}
	credential, err := identity.ParseCredential(payload)
	if err != nil {
		return CredentialIdentity{}, err
	}
	if err := v.Manager.VerifyCredential(credential, now); err != nil {
		if errors.Is(err, identity.ErrRevokedCredential) {
			return CredentialIdentity{}, fmt.Errorf("%w: %w", ErrCredentialRevoked, err)
		}
		return CredentialIdentity{}, err
	}
	return CredentialIdentity{
		AgentID: credential.AgentID, CredentialID: credential.CredentialID,
		ServerIdentityID: credential.ServerIdentityID,
	}, nil
}
