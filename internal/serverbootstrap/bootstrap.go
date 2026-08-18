// Package serverbootstrap opens the two independent Server persistence roots
// and establishes the canonical Audit Archive identity.
package serverbootstrap

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/serverstore"
)

const archiveSettingKey = "audit_archive_identity"

var (
	ErrArchiveRollback        = errors.New("audit archive generation is ahead of Server Identity State")
	ErrServerIdentityMismatch = errors.New("audit archive belongs to another Server Identity")
)

// ArchiveIdentity binds one operational database to the authoritative Server
// Identity State generation. It is stored in the operational DB, while the
// generation counter itself remains authoritative in the separate identity
// file.
type ArchiveIdentity struct {
	ServerIdentityID string `json:"server_identity_id"`
	Generation       uint64 `json:"archive_generation"`
	AuditArchiveID   string `json:"audit_archive_id"`
}

// Components are the durable Server foundations opened during boot.
type Components struct {
	Identity *identity.Manager
	Store    *serverstore.Store
	Archive  ArchiveIdentity
}

// Open creates or opens a secure Server state directory, then opens the
// identity and operational stores independently. A missing or restored-old
// operational database becomes a new Archive with a strictly higher durable
// generation.
func Open(ctx context.Context, stateDir string) (_ *Components, err error) {
	if err := ensureStateDir(stateDir); err != nil {
		return nil, err
	}
	manager, err := identity.Open(filepath.Join(stateDir, "identity", "server-identity.json"))
	if err != nil {
		return nil, fmt.Errorf("server bootstrap identity: %w", err)
	}
	store, err := serverstore.Open(ctx, filepath.Join(stateDir, "server.db"))
	if err != nil {
		return nil, fmt.Errorf("server bootstrap database: %w", err)
	}
	defer func() {
		if err != nil {
			_ = store.Close()
		}
	}()
	archive, err := reconcileArchive(ctx, manager, store.DB(), time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &Components{Identity: manager, Store: store, Archive: archive}, nil
}

// Close closes the operational database. Identity mutations are synchronously
// persisted and therefore need no close operation.
func (c *Components) Close() error {
	if c == nil || c.Store == nil {
		return nil
	}
	return c.Store.Close()
}

func ensureStateDir(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("server bootstrap state directory must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Server state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Server state directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Server state directory must not be a symlink or non-directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("Server state directory has writable group/other mode %04o", info.Mode().Perm())
	}
	return nil
}

func reconcileArchive(ctx context.Context, manager *identity.Manager, db *sql.DB, now time.Time) (result ArchiveIdentity, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ArchiveIdentity{}, fmt.Errorf("begin archive identity transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stored, found, err := loadArchive(ctx, tx)
	if err != nil {
		return ArchiveIdentity{}, err
	}
	serverID := manager.ServerIdentityID()
	currentGeneration := manager.ArchiveGeneration()
	if found {
		if stored.ServerIdentityID != serverID {
			return ArchiveIdentity{}, ErrServerIdentityMismatch
		}
		if stored.Generation > currentGeneration {
			return ArchiveIdentity{}, fmt.Errorf("%w: database=%d identity=%d", ErrArchiveRollback, stored.Generation, currentGeneration)
		}
		if stored.Generation == currentGeneration {
			if err := tx.Commit(); err != nil {
				return ArchiveIdentity{}, fmt.Errorf("commit archive identity read: %w", err)
			}
			return stored, nil
		}
	}

	generation, err := manager.AdvanceArchiveGeneration()
	if err != nil {
		return ArchiveIdentity{}, fmt.Errorf("advance archive generation: %w", err)
	}
	archiveID, err := randomID()
	if err != nil {
		return ArchiveIdentity{}, fmt.Errorf("generate audit archive id: %w", err)
	}
	result = ArchiveIdentity{
		ServerIdentityID: serverID,
		Generation:       generation,
		AuditArchiveID:   archiveID,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return ArchiveIdentity{}, fmt.Errorf("encode archive identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value_json = excluded.value_json,
			updated_at = excluded.updated_at
	`, archiveSettingKey, string(payload), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return ArchiveIdentity{}, fmt.Errorf("store archive identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ArchiveIdentity{}, fmt.Errorf("commit archive identity: %w", err)
	}
	return result, nil
}

func loadArchive(ctx context.Context, tx *sql.Tx) (ArchiveIdentity, bool, error) {
	var payload string
	err := tx.QueryRowContext(ctx,
		"SELECT value_json FROM settings WHERE key = ?", archiveSettingKey,
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ArchiveIdentity{}, false, nil
	}
	if err != nil {
		return ArchiveIdentity{}, false, fmt.Errorf("read archive identity: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var archive ArchiveIdentity
	if err := decoder.Decode(&archive); err != nil {
		return ArchiveIdentity{}, false, fmt.Errorf("decode archive identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ArchiveIdentity{}, false, errors.New("decode archive identity: trailing data")
	}
	if !validID(archive.ServerIdentityID) || archive.Generation == 0 || !validID(archive.AuditArchiveID) {
		return ArchiveIdentity{}, false, errors.New("decode archive identity: invalid fields")
	}
	return archive, true, nil
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func validID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
