// Package identity owns the Server Identity State and signed Agent credentials.
// It deliberately does not depend on the operational or audit database.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// StateFormatVersion is the on-disk Server Identity State format.
	StateFormatVersion uint32 = 1
	// CredentialVersion is the signed Agent credential format.
	CredentialVersion uint32 = 1
	// CredentialLifetime is the v1 lifetime fixed by architecture appendix B.2.
	CredentialLifetime = 90 * 24 * time.Hour
)

var (
	ErrInvalidState        = errors.New("invalid server identity state")
	ErrInsecurePermissions = errors.New("insecure server identity state permissions")
)

type signingKey struct {
	KeyID      string    `json:"key_id"`
	PublicKey  []byte    `json:"public_key"`
	PrivateKey []byte    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

// Revocation records trust state independently of the audit database.
type Revocation struct {
	CredentialID string    `json:"credential_id"`
	AgentID      string    `json:"agent_id"`
	RevokedAt    time.Time `json:"revoked_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Reason       string    `json:"reason"`
}

type persistedState struct {
	FormatVersion     uint32                 `json:"format_version"`
	ServerIdentityID  string                 `json:"server_identity_id"`
	CurrentKeyID      string                 `json:"current_key_id"`
	SigningKeys       map[string]*signingKey `json:"signing_keys"`
	ArchiveGeneration uint64                 `json:"archive_generation"`
	Revocations       []Revocation           `json:"revocation_ledger"`
}

// Manager serializes all mutations of the Server Identity State.
type Manager struct {
	mu    sync.RWMutex
	path  string
	state *persistedState
	hooks persistenceHooks
}

// Open loads an existing identity state or creates a new one.
func Open(path string) (*Manager, error) { return openAt(path, time.Now().UTC(), persistenceHooks{}) }

func openAt(path string, now time.Time, hooks persistenceHooks) (*Manager, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalidState)
	}
	m := &Manager{path: path, hooks: hooks}
	state, err := loadState(path)
	if err == nil {
		m.state = state
		return m, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	state, err = newState(now)
	if err != nil {
		return nil, err
	}
	m.state = state
	if _, err := m.saveLocked(state); err != nil {
		return nil, err
	}
	return m, nil
}

func newState(now time.Time) (*persistedState, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	identityID, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("generate server identity id: %w", err)
	}
	keyID, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("generate signing key id: %w", err)
	}
	now = canonicalTime(now)
	return &persistedState{
		FormatVersion:     StateFormatVersion,
		ServerIdentityID:  identityID,
		CurrentKeyID:      keyID,
		SigningKeys:       map[string]*signingKey{keyID: {KeyID: keyID, PublicKey: publicKey, PrivateKey: privateKey, CreatedAt: now}},
		ArchiveGeneration: 0,
		Revocations:       []Revocation{},
	}, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func loadState(path string) (*persistedState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: state path must be a regular file", ErrInvalidState)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o", ErrInsecurePermissions, path, info.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var state persistedState
	if err := dec.Decode(&state); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrInvalidState, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidState)
	}
	if err := validateState(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func validateState(state *persistedState) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidState, fmt.Sprintf(format, args...))
	}
	if state.FormatVersion != StateFormatVersion {
		return invalid("unsupported format version %d", state.FormatVersion)
	}
	if !validRandomID(state.ServerIdentityID) {
		return invalid("invalid server identity id")
	}
	if !validRandomID(state.CurrentKeyID) {
		return invalid("invalid current key id")
	}
	key := state.SigningKeys[state.CurrentKeyID]
	if key == nil {
		return invalid("current signing key is missing")
	}
	for id, candidate := range state.SigningKeys {
		if candidate == nil || id != candidate.KeyID || !validRandomID(id) {
			return invalid("invalid signing key entry %q", id)
		}
		if len(candidate.PublicKey) != ed25519.PublicKeySize || len(candidate.PrivateKey) != ed25519.PrivateKeySize {
			return invalid("invalid key material for %q", id)
		}
		derived, ok := ed25519.PrivateKey(candidate.PrivateKey).Public().(ed25519.PublicKey)
		if !ok || !bytes.Equal(derived, candidate.PublicKey) {
			return invalid("public/private signing key mismatch for %q", id)
		}
		if candidate.CreatedAt.IsZero() {
			return invalid("missing signing key creation time")
		}
	}
	seen := make(map[string]struct{}, len(state.Revocations))
	for _, revocation := range state.Revocations {
		if !validRandomID(revocation.CredentialID) || revocation.AgentID == "" || revocation.Reason == "" || revocation.RevokedAt.IsZero() || revocation.ExpiresAt.IsZero() {
			return invalid("incomplete revocation entry")
		}
		if !revocation.RevokedAt.Before(revocation.ExpiresAt) {
			return invalid("revocation is not before credential expiry")
		}
		if _, duplicate := seen[revocation.CredentialID]; duplicate {
			return invalid("duplicate revocation for credential %q", revocation.CredentialID)
		}
		seen[revocation.CredentialID] = struct{}{}
	}
	return nil
}

func validRandomID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func canonicalTime(t time.Time) time.Time { return t.UTC().Round(0) }

func cloneState(in *persistedState) *persistedState {
	out := *in
	out.SigningKeys = make(map[string]*signingKey, len(in.SigningKeys))
	for id, key := range in.SigningKeys {
		copyKey := *key
		copyKey.PublicKey = append([]byte(nil), key.PublicKey...)
		copyKey.PrivateKey = append([]byte(nil), key.PrivateKey...)
		out.SigningKeys[id] = &copyKey
	}
	out.Revocations = append([]Revocation(nil), in.Revocations...)
	return &out
}

// ServerIdentityID returns the stable identity for this Server installation.
func (m *Manager) ServerIdentityID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.ServerIdentityID
}

// CurrentKeyID returns the signing key selected for newly issued credentials.
func (m *Manager) CurrentKeyID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.CurrentKeyID
}

// CurrentPublicKey returns a defensive copy of the current public key.
func (m *Manager) CurrentPublicKey() ed25519.PublicKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append(ed25519.PublicKey(nil), m.state.SigningKeys[m.state.CurrentKeyID].PublicKey...)
}

// ArchiveGeneration returns the authoritative archive generation.
func (m *Manager) ArchiveGeneration() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.ArchiveGeneration
}

// Revocations returns a defensive copy of the revocation ledger.
func (m *Manager) Revocations() []Revocation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Revocation(nil), m.state.Revocations...)
}

// AdvanceArchiveGeneration durably advances the generation before returning.
func (m *Manager) AdvanceArchiveGeneration() (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.ArchiveGeneration == ^uint64(0) {
		return 0, fmt.Errorf("archive generation overflow")
	}
	next := cloneState(m.state)
	next.ArchiveGeneration++
	renamed, err := m.saveLocked(next)
	if renamed {
		m.state = next
	}
	if err != nil {
		return 0, err
	}
	m.state = next
	return next.ArchiveGeneration, nil
}

// CompactRevocations removes entries whose credentials have expired. The
// compaction is durable and never changes the active state on pre-rename errors.
func (m *Manager) CompactRevocations(now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now = canonicalTime(now)
	next := cloneState(m.state)
	kept := next.Revocations[:0]
	for _, entry := range next.Revocations {
		if now.Before(entry.ExpiresAt) {
			kept = append(kept, entry)
		}
	}
	removed := len(next.Revocations) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	next.Revocations = kept
	renamed, err := m.saveLocked(next)
	if renamed {
		m.state = next
	}
	if err != nil {
		return 0, err
	}
	m.state = next
	return removed, nil
}

type persistenceHooks struct {
	afterWrite    func() error
	afterFileSync func() error
	beforeRename  func() error
	beforeDirSync func() error
}

func callHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}

// saveLocked returns renamed=true once the new state is visible at the target
// path. A later directory fsync error is reported, while callers adopt the
// stricter newly-written trust state in memory.
func (m *Manager) saveLocked(state *persistedState) (renamed bool, err error) {
	if err := validateState(state); err != nil {
		return false, err
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode identity state: %w", err)
	}
	payload = append(payload, '\n')
	return atomicWrite(m.path, payload, m.hooks)
}

func atomicWrite(path string, payload []byte, hooks persistenceHooks) (renamed bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create secure state directory: %w", err)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return false, fmt.Errorf("inspect secure state directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return false, fmt.Errorf("%w: state directory must not be a symlink or non-directory", ErrInvalidState)
	}
	if dirInfo.Mode().Perm()&0o022 != 0 {
		return false, fmt.Errorf("%w: state directory %s has writable group/other mode %04o", ErrInsecurePermissions, dir, dirInfo.Mode().Perm())
	}
	tmp, err := os.CreateTemp(dir, ".server-identity-*")
	if err != nil {
		return false, fmt.Errorf("create secure state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return false, fmt.Errorf("secure state temp file: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		return false, fmt.Errorf("write secure state: %w", err)
	}
	if err := callHook(hooks.afterWrite); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("fsync secure state: %w", err)
	}
	if err := callHook(hooks.afterFileSync); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close secure state: %w", err)
	}
	if err := callHook(hooks.beforeRename); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("replace secure state: %w", err)
	}
	renamed = true
	if err := callHook(hooks.beforeDirSync); err != nil {
		return true, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return true, fmt.Errorf("open secure state directory for fsync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return true, fmt.Errorf("fsync secure state directory: %w", err)
	}
	return true, nil
}
