package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 15, 12, 30, 0, 123456789, time.FixedZone("KST", 9*60*60))

func TestOpenCreatesSecureStableServerIdentity(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "state", "server-identity.json")
	manager, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !validRandomID(manager.ServerIdentityID()) || !validRandomID(manager.CurrentKeyID()) {
		t.Fatalf("invalid generated ids: identity=%q key=%q", manager.ServerIdentityID(), manager.CurrentKeyID())
	}
	if manager.ArchiveGeneration() != 0 {
		t.Fatalf("initial archive generation = %d, want 0", manager.ArchiveGeneration())
	}
	if len(manager.CurrentPublicKey()) != ed25519.PublicKeySize {
		t.Fatalf("public key length = %d", len(manager.CurrentPublicKey()))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"private_key"`)) {
		t.Fatal("state did not persist private signing key")
	}
	reloaded, err := openAt(path, testNow.Add(time.Hour), persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ServerIdentityID() != manager.ServerIdentityID() || reloaded.CurrentKeyID() != manager.CurrentKeyID() || !bytes.Equal(reloaded.CurrentPublicKey(), manager.CurrentPublicKey()) {
		t.Fatal("identity changed across reload")
	}
}

func TestIssueVerifyRoundTripAndFixedLifetime(t *testing.T) {
	manager := newTestManager(t)
	credential, err := manager.IssueCredential("agent-17", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Version != CredentialVersion || credential.ServerIdentityID != manager.ServerIdentityID() || credential.KeyID != manager.CurrentKeyID() || !validRandomID(credential.CredentialID) {
		t.Fatalf("bad issued credential: %+v", credential)
	}
	if got := credential.ExpiresAt.Sub(credential.IssuedAt); got != CredentialLifetime {
		t.Fatalf("credential lifetime = %s, want %s", got, CredentialLifetime)
	}
	if err := manager.VerifyCredential(credential, credential.ExpiresAt.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("verify before expiry: %v", err)
	}
	if err := manager.VerifyCredential(credential, credential.ExpiresAt); !errors.Is(err, ErrExpiredCredential) {
		t.Fatalf("verify at expiry = %v, want expired", err)
	}
	payload, err := MarshalCredential(credential)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCredential(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyCredential(parsed, testNow); err != nil {
		t.Fatalf("verify parsed credential: %v", err)
	}
	if _, err := ParseCredential(append(payload, []byte(` {}`)...)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("parse trailing value = %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	unknown, _ := json.Marshal(object)
	if _, err := ParseCredential(unknown); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("parse unknown field = %v", err)
	}
}

func TestRenewalUsesHalfLifeAndKeepsOldCredentialValid(t *testing.T) {
	manager := newTestManager(t)
	old, err := manager.IssueCredential("agent-1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	threshold := old.IssuedAt.Add(CredentialLifetime / 2)
	if old.RenewalDue(threshold.Add(-time.Nanosecond)) {
		t.Fatal("renewal became due before half life")
	}
	if !old.RenewalDue(threshold) {
		t.Fatal("renewal was not due at half life")
	}
	replacement, err := manager.RenewCredential(old, threshold)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.CredentialID == old.CredentialID || replacement.AgentID != old.AgentID || replacement.IssuedAt != canonicalTime(threshold) {
		t.Fatalf("bad replacement: old=%+v new=%+v", old, replacement)
	}
	if err := manager.VerifyCredential(old, threshold); err != nil {
		t.Fatalf("old credential revoked before activation: %v", err)
	}
	if err := manager.VerifyCredential(replacement, threshold); err != nil {
		t.Fatalf("replacement credential invalid: %v", err)
	}
	if err := manager.RevokeCredential(old, threshold.Add(time.Minute), "replacement activated"); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyCredential(old, threshold.Add(time.Minute)); !errors.Is(err, ErrRevokedCredential) {
		t.Fatalf("old credential after activation revoke = %v", err)
	}
	if err := manager.VerifyCredential(replacement, threshold.Add(time.Minute)); err != nil {
		t.Fatalf("replacement affected by old revocation: %v", err)
	}
}

func TestCredentialFileAtomicRoundTripAndFault(t *testing.T) {
	dir := secureTempDir(t)
	path := filepath.Join(dir, "agent", "credential.json")
	manager := newTestManager(t)
	first, err := manager.IssueCredential("agent", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCredential(path, first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %04o", info.Mode().Perm())
	}
	loaded, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyCredential(loaded, testNow); err != nil {
		t.Fatalf("loaded credential invalid: %v", err)
	}
	second, err := manager.RenewCredential(first, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected replacement failure")
	hooks := persistenceHooks{beforeRename: func() error { return injected }}
	if _, err := saveCredentialWithHooks(path, second, hooks); !errors.Is(err, injected) {
		t.Fatalf("save fault = %v", err)
	}
	stillFirst, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if stillFirst.CredentialID != first.CredentialID {
		t.Fatal("pre-rename fault replaced active credential")
	}
	assertNoTemps(t, filepath.Dir(path))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("load insecure credential = %v", err)
	}
}

func TestVerifyRejectsEveryBoundFieldTamper(t *testing.T) {
	manager := newTestManager(t)
	credential, err := manager.IssueCredential("agent-1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Credential){
		"version":         func(c *Credential) { c.Version++ },
		"server identity": func(c *Credential) { c.ServerIdentityID = strings.Repeat("0", 32) },
		"agent":           func(c *Credential) { c.AgentID = "agent-2" },
		"credential id":   func(c *Credential) { c.CredentialID = strings.Repeat("1", 32) },
		"key id":          func(c *Credential) { c.KeyID = strings.Repeat("2", 32) },
		"issued at":       func(c *Credential) { c.IssuedAt = c.IssuedAt.Add(time.Second) },
		"expires at":      func(c *Credential) { c.ExpiresAt = c.ExpiresAt.Add(time.Second) },
		"signature":       func(c *Credential) { c.Signature[0] ^= 0xff },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tampered := cloneCredential(credential)
			mutate(&tampered)
			if err := manager.VerifyCredential(tampered, testNow); err == nil {
				t.Fatal("tampered credential verified")
			}
		})
	}
}

func TestVerifyRejectsCredentialFromOtherServer(t *testing.T) {
	first := newTestManager(t)
	second := newTestManager(t)
	credential, err := first.IssueCredential("agent", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.VerifyCredential(credential, testNow); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("other server verify = %v, want unknown key", err)
	}

	// Exercise the explicit identity check with a credential correctly signed by
	// this manager's key but carrying a different server identity.
	wrongIdentity := cloneCredential(credential)
	wrongIdentity.KeyID = second.CurrentKeyID()
	wrongIdentity.ServerIdentityID = first.ServerIdentityID()
	second.mu.RLock()
	key := second.state.SigningKeys[second.state.CurrentKeyID]
	second.mu.RUnlock()
	payload, err := wrongIdentity.signingPayload()
	if err != nil {
		t.Fatal(err)
	}
	wrongIdentity.Signature = ed25519.Sign(ed25519.PrivateKey(key.PrivateKey), payload)
	if err := second.VerifyCredential(wrongIdentity, testNow); !errors.Is(err, ErrWrongServerIdentity) {
		t.Fatalf("wrong identity verify = %v", err)
	}
}

func TestRevokeIsDurableIdempotentAndCompactable(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "identity.json")
	manager, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := manager.IssueCredential("agent-1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := testNow.Add(time.Hour)
	if err := manager.RevokeCredential(credential, revokedAt, "operator request"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeCredential(credential, revokedAt.Add(time.Minute), "duplicate"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	forged := cloneCredential(credential)
	forged.Signature[0] ^= 0xff
	if err := manager.RevokeCredential(forged, revokedAt.Add(time.Minute), "forged duplicate"); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("forged idempotent revoke = %v", err)
	}
	ledger := manager.Revocations()
	if len(ledger) != 1 || ledger[0].AgentID != credential.AgentID || ledger[0].CredentialID != credential.CredentialID || !ledger[0].RevokedAt.Equal(canonicalTime(revokedAt)) || !ledger[0].ExpiresAt.Equal(credential.ExpiresAt) || ledger[0].Reason != "operator request" {
		t.Fatalf("bad ledger: %+v", ledger)
	}
	if err := manager.VerifyCredential(credential, revokedAt); !errors.Is(err, ErrRevokedCredential) {
		t.Fatalf("verify revoked = %v", err)
	}
	reloaded, err := openAt(path, revokedAt, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.VerifyCredential(credential, revokedAt); !errors.Is(err, ErrRevokedCredential) {
		t.Fatalf("verify after reload = %v", err)
	}
	if removed, err := reloaded.CompactRevocations(credential.ExpiresAt); err != nil || removed != 1 {
		t.Fatalf("compact = %d, %v", removed, err)
	}
	if len(reloaded.Revocations()) != 0 {
		t.Fatal("expired revocation was not removed")
	}
}

func TestRevokePreRenameFaultPreservesOldState(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "identity.json")
	manager, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := manager.IssueCredential("agent", testNow)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected before rename")
	manager.hooks.beforeRename = func() error { return injected }
	if err := manager.RevokeCredential(credential, testNow.Add(time.Hour), "test"); !errors.Is(err, injected) {
		t.Fatalf("revoke error = %v", err)
	}
	if len(manager.Revocations()) != 0 {
		t.Fatal("failed pre-rename revoke changed memory")
	}
	reloaded, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.VerifyCredential(credential, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("old durable state not preserved: %v", err)
	}
	assertNoTemps(t, filepath.Dir(path))
}

func TestRevokePostRenameFaultAdoptsStricterState(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "identity.json")
	manager, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := manager.IssueCredential("agent", testNow)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory fsync failure")
	manager.hooks.beforeDirSync = func() error { return injected }
	if err := manager.RevokeCredential(credential, testNow.Add(time.Hour), "test"); !errors.Is(err, injected) {
		t.Fatalf("revoke error = %v", err)
	}
	if err := manager.VerifyCredential(credential, testNow.Add(time.Hour)); !errors.Is(err, ErrRevokedCredential) {
		t.Fatalf("post-rename memory must fail closed, got %v", err)
	}
	reloaded, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.VerifyCredential(credential, testNow.Add(time.Hour)); !errors.Is(err, ErrRevokedCredential) {
		t.Fatalf("renamed state missing revocation: %v", err)
	}
	assertNoTemps(t, filepath.Dir(path))
}

func TestEveryPreRenameStageFailureCleansTemp(t *testing.T) {
	for name, configure := range map[string]func(*persistenceHooks, error){
		"write":     func(h *persistenceHooks, err error) { h.afterWrite = func() error { return err } },
		"file sync": func(h *persistenceHooks, err error) { h.afterFileSync = func() error { return err } },
		"rename":    func(h *persistenceHooks, err error) { h.beforeRename = func() error { return err } },
	} {
		t.Run(name, func(t *testing.T) {
			dir := secureTempDir(t)
			path := filepath.Join(dir, "identity.json")
			injected := errors.New("injected " + name)
			hooks := persistenceHooks{}
			configure(&hooks, injected)
			if _, err := openAt(path, testNow, hooks); !errors.Is(err, injected) {
				t.Fatalf("open error = %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state unexpectedly visible: %v", err)
			}
			assertNoTemps(t, dir)
		})
	}
}

func TestAdvanceArchiveGenerationDurabilityAndFaultBoundary(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "identity.json")
	manager, err := openAt(path, testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if generation, err := manager.AdvanceArchiveGeneration(); err != nil || generation != 1 {
		t.Fatalf("advance = %d, %v", generation, err)
	}
	reloaded, err := openAt(path, testNow, persistenceHooks{})
	if err != nil || reloaded.ArchiveGeneration() != 1 {
		t.Fatalf("reloaded generation = %d, %v", reloaded.ArchiveGeneration(), err)
	}
	injected := errors.New("injected")
	reloaded.hooks.beforeRename = func() error { return injected }
	if _, err := reloaded.AdvanceArchiveGeneration(); !errors.Is(err, injected) {
		t.Fatalf("fault advance error = %v", err)
	}
	if reloaded.ArchiveGeneration() != 1 {
		t.Fatal("pre-rename fault advanced memory")
	}
	again, err := openAt(path, testNow, persistenceHooks{})
	if err != nil || again.ArchiveGeneration() != 1 {
		t.Fatalf("pre-rename fault advanced disk: generation=%d err=%v", again.ArchiveGeneration(), err)
	}
}

func TestLoadRejectsInsecureCorruptAndSymlinkState(t *testing.T) {
	t.Run("permissions", func(t *testing.T) {
		path := filepath.Join(secureTempDir(t), "identity.json")
		manager, err := openAt(path, testNow, persistenceHooks{})
		if err != nil {
			t.Fatal(err)
		}
		_ = manager
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("open insecure mode = %v", err)
		}
	})
	t.Run("corrupt key", func(t *testing.T) {
		path := filepath.Join(secureTempDir(t), "identity.json")
		manager, err := openAt(path, testNow, persistenceHooks{})
		if err != nil {
			t.Fatal(err)
		}
		manager.mu.RLock()
		state := cloneState(manager.state)
		manager.mu.RUnlock()
		state.SigningKeys[state.CurrentKeyID].PublicKey[0] ^= 0xff
		payload, _ := json.Marshal(state)
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("open corrupt key = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := secureTempDir(t)
		realPath := filepath.Join(dir, "real.json")
		if _, err := openAt(realPath, testNow, persistenceHooks{}); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(dir, "link.json")
		if err := os.Symlink(realPath, linkPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(linkPath); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("open symlink = %v", err)
		}
	})
}

func TestAtomicStateRejectsWritableOrSymlinkDirectory(t *testing.T) {
	t.Run("writable directory", func(t *testing.T) {
		dir := secureTempDir(t)
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(dir, "identity.json")); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("Open in writable directory error = %v", err)
		}
	})

	t.Run("symlink directory", func(t *testing.T) {
		parent := secureTempDir(t)
		realDir := filepath.Join(parent, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(parent, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(linkDir, "identity.json")); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("Open through symlink directory error = %v", err)
		}
	})
}

func TestConcurrentIssueAndVerify(t *testing.T) {
	manager := newTestManager(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credential, err := manager.IssueCredential("agent", testNow)
			if err == nil {
				err = manager.VerifyCredential(credential, testNow)
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := openAt(filepath.Join(secureTempDir(t), "identity.json"), testNow, persistenceHooks{})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func cloneCredential(in Credential) Credential {
	out := in
	out.Signature = append([]byte(nil), in.Signature...)
	return out
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".server-identity-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}

// secureTempDir returns a temporary directory the identity manager may adopt.
// t.TempDir inherits the umask the suite was launched with, and identity state
// that is group- or other-writable is refused by design, so the mode is made
// explicit here rather than depending on how the suite was invoked.
func secureTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
