package producttransport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/identity"
)

func TestIdentityVerifierChecksDurableRevocation(t *testing.T) {
	manager, err := identity.Open(filepath.Join(secureIdentityDir(t), "identity.json"))
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

// secureIdentityDir returns a temporary directory the identity manager may
// adopt. t.TempDir inherits the umask the suite was launched with, and
// identity state that is group- or other-writable is refused by design.
func secureIdentityDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
