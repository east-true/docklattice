package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var (
	ErrInvalidCredential   = errors.New("invalid agent credential")
	ErrInvalidSignature    = errors.New("invalid agent credential signature")
	ErrWrongServerIdentity = errors.New("agent credential belongs to another server identity")
	ErrUnknownSigningKey   = errors.New("unknown agent credential signing key")
	ErrExpiredCredential   = errors.New("agent credential expired")
	ErrRevokedCredential   = errors.New("agent credential revoked")
)

// Credential is a self-contained Server-signed Agent credential.
type Credential struct {
	Version          uint32    `json:"version"`
	ServerIdentityID string    `json:"server_identity_id"`
	AgentID          string    `json:"agent_id"`
	CredentialID     string    `json:"credential_id"`
	KeyID            string    `json:"key_id"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Signature        []byte    `json:"signature"`
}

// IssueCredential issues a credential with the fixed v1 90-day lifetime.
func (m *Manager) IssueCredential(agentID string, now time.Time) (Credential, error) {
	if agentID == "" || len(agentID) > 1024 {
		return Credential{}, fmt.Errorf("%w: invalid agent id", ErrInvalidCredential)
	}
	now = canonicalTime(now)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return issueCredential(m.state, agentID, now)
}

func issueCredential(state *persistedState, agentID string, now time.Time) (Credential, error) {
	credentialID, err := randomID()
	if err != nil {
		return Credential{}, fmt.Errorf("generate credential id: %w", err)
	}
	key := state.SigningKeys[state.CurrentKeyID]
	credential := Credential{
		Version:          CredentialVersion,
		ServerIdentityID: state.ServerIdentityID,
		AgentID:          agentID,
		CredentialID:     credentialID,
		KeyID:            key.KeyID,
		IssuedAt:         now,
		ExpiresAt:        now.Add(CredentialLifetime),
	}
	payload, err := credential.signingPayload()
	if err != nil {
		return Credential{}, err
	}
	credential.Signature = ed25519.Sign(ed25519.PrivateKey(key.PrivateKey), payload)
	return credential, nil
}

// RenewCredential authenticates the current credential and issues a distinct
// replacement. It intentionally leaves the old credential valid; callers must
// wait for Agent activation acknowledgement before revoking it (§6.3).
func (m *Manager) RenewCredential(current Credential, now time.Time) (Credential, error) {
	now = canonicalTime(now)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := verifyCredential(m.state, current, now, true); err != nil {
		return Credential{}, err
	}
	return issueCredential(m.state, current.AgentID, now)
}

// RenewalDue reports the v1 50% lifetime renewal threshold.
func (credential Credential) RenewalDue(now time.Time) bool {
	if credential.IssuedAt.IsZero() || credential.ExpiresAt.IsZero() || !credential.ExpiresAt.After(credential.IssuedAt) {
		return false
	}
	threshold := credential.IssuedAt.Add(credential.ExpiresAt.Sub(credential.IssuedAt) / 2)
	return !canonicalTime(now).Before(threshold)
}

// VerifyCredential verifies signature, server identity, expiry, and revocation.
func (m *Manager) VerifyCredential(credential Credential, now time.Time) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return verifyCredential(m.state, credential, canonicalTime(now), true)
}

func verifyCredential(state *persistedState, credential Credential, now time.Time, checkRevocation bool) error {
	if credential.Version != CredentialVersion || credential.AgentID == "" || credential.CredentialID == "" || credential.KeyID == "" || credential.IssuedAt.IsZero() || credential.ExpiresAt.IsZero() || !credential.ExpiresAt.After(credential.IssuedAt) {
		return ErrInvalidCredential
	}
	key := state.SigningKeys[credential.KeyID]
	if key == nil {
		return ErrUnknownSigningKey
	}
	payload, err := credential.signingPayload()
	if err != nil {
		return err
	}
	if len(credential.Signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(key.PublicKey), payload, credential.Signature) {
		return ErrInvalidSignature
	}
	if subtle.ConstantTimeCompare([]byte(credential.ServerIdentityID), []byte(state.ServerIdentityID)) != 1 {
		return ErrWrongServerIdentity
	}
	if !now.Before(credential.ExpiresAt) {
		return ErrExpiredCredential
	}
	if checkRevocation {
		for _, revoked := range state.Revocations {
			if subtle.ConstantTimeCompare([]byte(revoked.CredentialID), []byte(credential.CredentialID)) == 1 {
				if revoked.AgentID != credential.AgentID {
					return ErrInvalidCredential
				}
				return ErrRevokedCredential
			}
		}
	}
	return nil
}

// RevokeCredential durably records revocation before returning success.
func (m *Manager) RevokeCredential(credential Credential, now time.Time, reason string) error {
	if reason == "" {
		return fmt.Errorf("%w: empty revocation reason", ErrInvalidCredential)
	}
	now = canonicalTime(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := verifyCredential(m.state, credential, now, false); err != nil {
		return err
	}
	for _, revoked := range m.state.Revocations {
		if revoked.CredentialID == credential.CredentialID {
			if revoked.AgentID != credential.AgentID {
				return ErrInvalidCredential
			}
			return nil
		}
	}
	next := cloneState(m.state)
	next.Revocations = append(next.Revocations, Revocation{
		CredentialID: credential.CredentialID,
		AgentID:      credential.AgentID,
		RevokedAt:    now,
		ExpiresAt:    credential.ExpiresAt,
		Reason:       reason,
	})
	renamed, err := m.saveLocked(next)
	if renamed {
		// Once rename occurred, use the stricter trust state even if directory
		// fsync reports an ambiguous durability error.
		m.state = next
	}
	if err != nil {
		return err
	}
	m.state = next
	return nil
}

// MarshalCredential returns the stable JSON representation used by Agents.
func MarshalCredential(credential Credential) ([]byte, error) {
	if _, err := credential.signingPayload(); err != nil {
		return nil, err
	}
	if len(credential.Signature) != ed25519.SignatureSize {
		return nil, ErrInvalidSignature
	}
	return json.Marshal(credential)
}

// ParseCredential strictly parses one credential JSON value.
func ParseCredential(payload []byte) (Credential, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var credential Credential
	if err := dec.Decode(&credential); err != nil {
		return Credential{}, fmt.Errorf("%w: decode: %v", ErrInvalidCredential, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Credential{}, fmt.Errorf("%w: trailing data", ErrInvalidCredential)
	}
	if _, err := credential.signingPayload(); err != nil {
		return Credential{}, err
	}
	if len(credential.Signature) != ed25519.SignatureSize {
		return Credential{}, ErrInvalidSignature
	}
	return credential, nil
}

// SaveCredential performs the Agent-side temp+file-fsync+rename+dir-fsync
// sequence. Credentials are bearer secrets and are stored with mode 0600.
func SaveCredential(path string, credential Credential) error {
	_, err := saveCredentialWithHooks(path, credential, persistenceHooks{})
	return err
}

func saveCredentialWithHooks(path string, credential Credential, hooks persistenceHooks) (bool, error) {
	payload, err := MarshalCredential(credential)
	if err != nil {
		return false, err
	}
	payload = append(payload, '\n')
	return atomicWrite(path, payload, hooks)
}

// LoadCredential loads a regular, owner-only credential file.
func LoadCredential(path string) (Credential, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Credential{}, err
	}
	if !info.Mode().IsRegular() {
		return Credential{}, fmt.Errorf("%w: credential path must be a regular file", ErrInvalidCredential)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credential{}, fmt.Errorf("%w: credential file has mode %04o", ErrInsecurePermissions, info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, err
	}
	return ParseCredential(payload)
}

func (credential Credential) signingPayload() ([]byte, error) {
	if credential.Version == 0 ||
		!validRandomID(credential.ServerIdentityID) ||
		credential.AgentID == "" || len(credential.AgentID) > 1024 ||
		!validRandomID(credential.CredentialID) ||
		!validRandomID(credential.KeyID) ||
		credential.IssuedAt.IsZero() || credential.ExpiresAt.IsZero() ||
		!credential.ExpiresAt.After(credential.IssuedAt) {
		return nil, ErrInvalidCredential
	}
	var out bytes.Buffer
	out.WriteString("docklattice-agent-credential\x00")
	_ = binary.Write(&out, binary.BigEndian, credential.Version)
	for _, value := range []string{credential.ServerIdentityID, credential.AgentID, credential.CredentialID, credential.KeyID} {
		if len(value) > int(^uint32(0)) {
			return nil, ErrInvalidCredential
		}
		_ = binary.Write(&out, binary.BigEndian, uint32(len(value)))
		out.WriteString(value)
	}
	_ = binary.Write(&out, binary.BigEndian, credential.IssuedAt.UnixNano())
	_ = binary.Write(&out, binary.BigEndian, credential.ExpiresAt.UnixNano())
	return out.Bytes(), nil
}
