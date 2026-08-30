// Package registration implements one-time Join Tokens and Agent self-registration.
package registration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/agentid"
	"github.com/east-true/docklattice/internal/identity"
	"github.com/east-true/docklattice/internal/serverstore"
)

const (
	JoinTokenSecretBytes = 32
	TokenIDRandomBytes   = 16

	MaxDisplayNameBytes   = 128
	MaxMetadataBytes      = 4096
	MaxMetadataEntries    = 32
	MaxMetadataKeyBytes   = 64
	MaxMetadataValueBytes = 512
)

var (
	ErrInvalidJoinToken   = errors.New("invalid or unavailable join token")
	ErrInvalidRequest     = errors.New("invalid registration request")
	ErrIdentityReuseProof = errors.New("existing agent identity reuse proof rejected")
	ErrAgentRetired       = errors.New("agent identity is retired")
	ErrAgentIDCollision   = errors.New("agent identity is already registered")
)

const databaseTimeFormat = "2006-01-02T15:04:05.000000000Z"

// IssuedToken contains the only plaintext copy returned by the Server.
type IssuedToken struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

// ReuseRequest explicitly asks to preserve an existing stable Agent identity.
// A purpose-bound Rejoin Token and the prior signed, expired credential are
// both required; a normal Join Token can never select an existing Agent ID.
type ReuseRequest struct {
	ExpiredCredential identity.Credential
}

type Request struct {
	JoinToken   string
	AgentID     string
	DisplayName string
	Metadata    map[string]string
	Reuse       *ReuseRequest
}

type Result struct {
	AgentID    string
	Credential identity.Credential
}

type Service struct {
	store      *serverstore.Store
	identities *identity.Manager
	now        func() time.Time
	random     io.Reader

	// Tests use this hook to prove that token consumption rolls back when a
	// later registration step fails.
	afterConsume func() error
}

func New(store *serverstore.Store, identities *identity.Manager) (*Service, error) {
	if store == nil || store.DB() == nil || identities == nil {
		return nil, errors.New("registration: store and identity manager are required")
	}
	return &Service{store: store, identities: identities, now: time.Now, random: rand.Reader}, nil
}

// IssueJoinToken creates a general-purpose token for a new Agent identity.
func (s *Service) IssueJoinToken(ctx context.Context, lifetime time.Duration) (IssuedToken, error) {
	return s.issueToken(ctx, "join", "", lifetime)
}

// IssueRejoinToken creates a token bound to one existing Agent ID. Binding is
// encoded in the non-secret token ID so the existing schema still stores only
// the SHA-256 digest of the 256-bit secret.
func (s *Service) IssueRejoinToken(ctx context.Context, agentID string, lifetime time.Duration) (IssuedToken, error) {
	if !agentid.Valid(agentID) {
		return IssuedToken{}, fmt.Errorf("%w: invalid rejoin agent id", ErrInvalidRequest)
	}
	return s.issueToken(ctx, "rejoin", agentID, lifetime)
}

func (s *Service) issueToken(ctx context.Context, kind, boundAgentID string, lifetime time.Duration) (IssuedToken, error) {
	if lifetime <= 0 {
		return IssuedToken{}, fmt.Errorf("%w: token lifetime must be positive", ErrInvalidRequest)
	}
	now := canonicalTime(s.now())
	expiresAt := now.Add(lifetime)
	randomID, err := randomHex(s.random, TokenIDRandomBytes)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("registration: generate token id: %w", err)
	}
	tokenID := kind + "_" + randomID
	if kind == "rejoin" {
		tokenID = kind + "_" + base64.RawURLEncoding.EncodeToString([]byte(boundAgentID)) + "_" + randomID
	}
	secret := make([]byte, JoinTokenSecretBytes)
	if _, err := io.ReadFull(s.random, secret); err != nil {
		return IssuedToken{}, fmt.Errorf("registration: generate token secret: %w", err)
	}
	digest := sha256.Sum256(secret)
	if _, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO join_tokens(id, hash, created_at, expires_at)
		VALUES (?, ?, ?, ?)
	`, tokenID, digest[:], databaseTime(now), databaseTime(expiresAt)); err != nil {
		return IssuedToken{}, fmt.Errorf("registration: persist join token: %w", err)
	}
	plaintext := tokenID + "." + base64.RawURLEncoding.EncodeToString(secret)
	return IssuedToken{ID: tokenID, Token: plaintext, ExpiresAt: expiresAt}, nil
}

// RevokeJoinToken atomically marks an unconsumed token unavailable.
func (s *Service) RevokeJoinToken(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return ErrInvalidJoinToken
	}
	tx, err := s.store.DB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("registration: begin token revocation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE join_tokens
		SET revoked_at = COALESCE(revoked_at, ?)
		WHERE id = ? AND consumed_at IS NULL
	`, databaseTime(canonicalTime(s.now())), tokenID)
	if err != nil {
		return fmt.Errorf("registration: revoke join token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("registration: inspect token revocation: %w", err)
	}
	if rows != 1 {
		return ErrInvalidJoinToken
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registration: commit token revocation: %w", err)
	}
	return nil
}

// Register consumes exactly one token in the same transaction that creates or
// restores the Agent registry row. No credential is returned unless commit
// succeeds.
func (s *Service) Register(ctx context.Context, request Request) (Result, error) {
	metadataJSON, err := validateRegistrationFields(request.DisplayName, request.Metadata)
	if err != nil {
		return Result{}, err
	}
	now := canonicalTime(s.now())
	tokenID, secret, tokenKind, boundAgentID, err := parsePresentedToken(request.JoinToken)
	if err != nil {
		return Result{}, ErrInvalidJoinToken
	}
	if !agentid.Valid(request.AgentID) {
		return Result{}, fmt.Errorf("%w: agent_id must be a canonical UUIDv4", ErrInvalidRequest)
	}

	if request.Reuse == nil {
		if tokenKind != "join" {
			return Result{}, ErrIdentityReuseProof
		}
	} else {
		if tokenKind != "rejoin" || boundAgentID != request.AgentID {
			return Result{}, ErrIdentityReuseProof
		}
		if err := s.verifyReuse(request.AgentID, *request.Reuse, now); err != nil {
			return Result{}, err
		}
	}

	tx, err := s.store.DB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Result{}, fmt.Errorf("registration: begin: %w", err)
	}
	defer tx.Rollback()
	if err := consumeToken(ctx, tx, tokenID, secret, now); err != nil {
		return Result{}, err
	}
	if s.afterConsume != nil {
		if err := s.afterConsume(); err != nil {
			return Result{}, err
		}
	}

	if request.Reuse == nil {
		inserted, err := tx.ExecContext(ctx, `
			INSERT INTO agents(id, display_name, first_seen_at, last_seen_at, metadata_json, capabilities_json)
			VALUES (?, ?, ?, ?, ?, '{}')
			ON CONFLICT(id) DO NOTHING
		`, request.AgentID, request.DisplayName, databaseTime(now), databaseTime(now), metadataJSON)
		if err != nil {
			return Result{}, fmt.Errorf("registration: create agent: %w", err)
		}
		rows, err := inserted.RowsAffected()
		if err != nil {
			return Result{}, fmt.Errorf("registration: inspect agent creation: %w", err)
		}
		if rows != 1 {
			return Result{}, ErrAgentIDCollision
		}
	} else if err := restoreAgentRow(ctx, tx, request.AgentID, request.DisplayName, metadataJSON, now); err != nil {
		return Result{}, err
	}

	credential, err := s.identities.IssueCredential(request.AgentID, now)
	if err != nil {
		return Result{}, fmt.Errorf("registration: issue credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("registration: commit: %w", err)
	}
	return Result{AgentID: request.AgentID, Credential: credential}, nil
}

func consumeToken(ctx context.Context, tx *sql.Tx, tokenID string, secret []byte, now time.Time) error {
	calculated := sha256.Sum256(secret)
	var storedHash []byte
	var expiresAt string
	var consumedAt, revokedAt sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT hash, expires_at, consumed_at, revoked_at
		FROM join_tokens WHERE id = ?
	`, tokenID).Scan(&storedHash, &expiresAt, &consumedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var dummy [sha256.Size]byte
		_ = subtle.ConstantTimeCompare(dummy[:], calculated[:])
		return ErrInvalidJoinToken
	}
	if err != nil {
		return fmt.Errorf("registration: read join token: %w", err)
	}
	if len(storedHash) != sha256.Size || subtle.ConstantTimeCompare(storedHash, calculated[:]) != 1 {
		return ErrInvalidJoinToken
	}
	expires, err := time.Parse(databaseTimeFormat, expiresAt)
	if err != nil {
		return fmt.Errorf("registration: corrupt join token expiry: %w", err)
	}
	if consumedAt.Valid || revokedAt.Valid || !now.Before(expires) {
		return ErrInvalidJoinToken
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE join_tokens SET consumed_at = ?
		WHERE id = ? AND hash = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?
	`, databaseTime(now), tokenID, calculated[:], databaseTime(now))
	if err != nil {
		return fmt.Errorf("registration: consume join token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("registration: inspect token consumption: %w", err)
	}
	if rows != 1 {
		return ErrInvalidJoinToken
	}
	return nil
}

func restoreAgentRow(ctx context.Context, tx *sql.Tx, agentID, displayName, metadataJSON string, now time.Time) error {
	var retiredAt sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT retired_at FROM agents WHERE id = ?", agentID).Scan(&retiredAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registration: inspect existing agent: %w", err)
	}
	if retiredAt.Valid {
		return ErrAgentRetired
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO agents(id, display_name, first_seen_at, last_seen_at, metadata_json, capabilities_json)
			VALUES (?, ?, ?, ?, ?, '{}')
		`, agentID, displayName, databaseTime(now), databaseTime(now), metadataJSON)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE agents SET display_name = ?, last_seen_at = ?, metadata_json = ? WHERE id = ?
		`, displayName, databaseTime(now), metadataJSON, agentID)
	}
	if err != nil {
		return fmt.Errorf("registration: restore agent: %w", err)
	}
	return nil
}

func (s *Service) verifyReuse(requestedAgentID string, reuse ReuseRequest, now time.Time) error {
	credential := reuse.ExpiredCredential
	if !agentid.Valid(requestedAgentID) || credential.AgentID != requestedAgentID {
		return ErrIdentityReuseProof
	}
	err := s.identities.VerifyCredential(credential, now)
	if !errors.Is(err, identity.ErrExpiredCredential) {
		return ErrIdentityReuseProof
	}
	for _, revoked := range s.identities.Revocations() {
		if subtle.ConstantTimeCompare([]byte(revoked.CredentialID), []byte(credential.CredentialID)) == 1 {
			return ErrIdentityReuseProof
		}
	}
	return nil
}

func validateRegistrationFields(displayName string, metadata map[string]string) (string, error) {
	if displayName == "" || !utf8.ValidString(displayName) || len(displayName) > MaxDisplayNameBytes {
		return "", fmt.Errorf("%w: invalid display name", ErrInvalidRequest)
	}
	if len(metadata) > MaxMetadataEntries {
		return "", fmt.Errorf("%w: too many metadata entries", ErrInvalidRequest)
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	for key, value := range metadata {
		if key == "" || !utf8.ValidString(key) || !utf8.ValidString(value) || len(key) > MaxMetadataKeyBytes || len(value) > MaxMetadataValueBytes {
			return "", fmt.Errorf("%w: invalid metadata", ErrInvalidRequest)
		}
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("%w: encode metadata: %v", ErrInvalidRequest, err)
	}
	if len(payload) > MaxMetadataBytes {
		return "", fmt.Errorf("%w: metadata too large", ErrInvalidRequest)
	}
	return string(payload), nil
}

func parsePresentedToken(token string) (id string, secret []byte, kind, boundAgentID string, err error) {
	if strings.Count(token, ".") != 1 {
		return "", nil, "", "", ErrInvalidJoinToken
	}
	id, encodedSecret, _ := strings.Cut(token, ".")
	secret, err = base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil || len(secret) != JoinTokenSecretBytes {
		return "", nil, "", "", ErrInvalidJoinToken
	}
	if strings.HasPrefix(id, "join_") {
		randomPart := strings.TrimPrefix(id, "join_")
		if !validHex(randomPart, TokenIDRandomBytes) {
			return "", nil, "", "", ErrInvalidJoinToken
		}
		return id, secret, "join", "", nil
	}
	if strings.HasPrefix(id, "rejoin_") {
		rest := strings.TrimPrefix(id, "rejoin_")
		separator := strings.LastIndexByte(rest, '_')
		if separator <= 0 || !validHex(rest[separator+1:], TokenIDRandomBytes) {
			return "", nil, "", "", ErrInvalidJoinToken
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(rest[:separator])
		if decodeErr != nil || !agentid.Valid(string(decoded)) {
			return "", nil, "", "", ErrInvalidJoinToken
		}
		return id, secret, "rejoin", string(decoded), nil
	}
	return "", nil, "", "", ErrInvalidJoinToken
}

func validHex(value string, bytes int) bool {
	if len(value) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func randomHex(reader io.Reader, bytes int) (string, error) {
	payload := make([]byte, bytes)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", err
	}
	return hex.EncodeToString(payload), nil
}

func canonicalTime(value time.Time) time.Time { return value.UTC().Round(0) }
func databaseTime(value time.Time) string     { return canonicalTime(value).Format(databaseTimeFormat) }
