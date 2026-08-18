package producttransport

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/identity"
)

func TestIdentityVerifierChecksDurableRevocation(t *testing.T) {
	manager, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(0)
	credential, err := manager.IssueCredential("agent-real", now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := identity.MarshalCredential(credential)
	if err != nil {
		t.Fatal(err)
	}
	verifier := IdentityVerifier{Manager: manager}
	verified, err := verifier.VerifyCredential(context.Background(), payload, now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.AgentID != credential.AgentID || verified.CredentialID != credential.CredentialID || verified.ServerIdentityID != credential.ServerIdentityID {
		t.Fatalf("verified identity = %#v", verified)
	}
	if err := manager.RevokeCredential(credential, now.Add(time.Second), "test revocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyCredential(context.Background(), payload, now.Add(2*time.Second)); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("revoked verification error = %v", err)
	}
}
