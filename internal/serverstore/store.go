// Package serverstore owns Dockpilot's canonical Server SQLite database.
package serverstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	sqlite "modernc.org/sqlite"
)

// ErrBusy reports SQLite write contention: the write lock was held by another
// transaction for longer than busy_timeout allows. It is load, not a broken
// invariant, and callers must be able to say so.
var ErrBusy = errors.New("serverstore: database is busy")

// Busy reports whether err is SQLite write contention in any of its forms,
// including the extended busy codes.
func Busy(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == sqliteBusyCode
	}
	return errors.Is(err, ErrBusy)
}

const (
	// CurrentSchemaVersion is the newest schema understood by this binary.
	CurrentSchemaVersion = 6
	busyTimeoutMillis    = 5000
	sqliteBusyCode       = 5
)

// Store is the product Server's persistent SQLite store.
//
// It keeps two pools over the same file. db serves reads and single-statement
// writes; writer serves transactions that are known to write. A transaction
// that reads before it writes cannot be started DEFERRED: when another
// connection commits in between, SQLite refuses the upgrade with SQLITE_BUSY
// and does not consult busy_timeout, because the reader's snapshot is already
// stale and waiting could never help. The writer pool therefore begins every
// transaction IMMEDIATE. It is a separate pool rather than a global DSN option
// so read-only work is never made to queue behind writers.
type Store struct {
	db     *sql.DB
	writer *sql.DB
}

// Open opens (or creates) a file-backed Server database and applies all known
// migrations. SQLite connection pragmas are encoded in the DSN so they apply
// to every connection opened by database/sql, not only the first connection.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("serverstore: database path is empty")
	}
	if path == ":memory:" {
		return nil, errors.New("serverstore: database must be file-backed")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("serverstore: resolve database path: %w", err)
	}
	if err := ensureSecureDatabaseFile(absPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", sqliteDSN(absPath))
	if err != nil {
		return nil, fmt.Errorf("serverstore: open database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("serverstore: ping database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	writer, err := sql.Open("sqlite", sqliteWriteDSN(absPath))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("serverstore: open write pool: %w", err)
	}
	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		db.Close()
		return nil, fmt.Errorf("serverstore: ping write pool: %w", err)
	}

	return &Store{db: db, writer: writer}, nil
}

func ensureSecureDatabaseFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("serverstore: create database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("serverstore: close database file: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("serverstore: inspect database file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("serverstore: database path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("serverstore: database file permissions %04o expose Server state", info.Mode().Perm())
	}
	return nil
}

func sqliteDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	q.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = q.Encode()
	return u.String()
}

// sqliteWriteDSN is the same connection with BEGIN IMMEDIATE as its
// transaction mode. Only the write pool uses it.
func sqliteWriteDSN(path string) string {
	return sqliteDSN(path) + "&_txlock=immediate"
}

// BeginWrite starts a transaction that takes the write lock before its first
// statement. Every transaction that will write - including one that reads to
// decide what to write - must start here rather than with DB().BeginTx, whose
// DEFERRED transactions cannot be upgraded once another writer has committed.
func (s *Store) BeginWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		if Busy(err) {
			return nil, fmt.Errorf("%w: write lock unavailable", ErrBusy)
		}
		return nil, fmt.Errorf("serverstore: begin write transaction: %w", err)
	}
	return tx, nil
}

// DB exposes the connection pool to repository implementations in this
// internal package tree. Callers must not change connection pragmas.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Close closes both connection pools.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	var writerErr error
	if s.writer != nil {
		writerErr = s.writer.Close()
	}
	if err := s.db.Close(); err != nil {
		return err
	}
	return writerErr
}

// SchemaVersion returns SQLite's transactional user_version marker.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("serverstore: read schema version: %w", err)
	}
	return version, nil
}

// LoadIncarnation returns the highest Agent incarnation durably accepted by
// this Server. It implements producttransport.IncarnationWatermarkStore
// without making serverstore depend on the transport package.
func (s *Store) LoadIncarnation(ctx context.Context, agentID string) (uint64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("serverstore: store is closed")
	}
	if agentID == "" {
		return 0, errors.New("serverstore: agent id is empty")
	}
	var value int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT last_incarnation FROM agents WHERE id = ?", agentID,
	).Scan(&value); err != nil {
		return 0, fmt.Errorf("serverstore: load Agent incarnation: %w", err)
	}
	if value < 0 {
		return 0, errors.New("serverstore: corrupt negative Agent incarnation")
	}
	return uint64(value), nil
}

// CompareAndSwapIncarnation advances an Agent's durable session-replay
// watermark exactly when the expected value still matches. The single UPDATE
// is atomic across database connections and Server processes.
func (s *Store) CompareAndSwapIncarnation(ctx context.Context, agentID string, old, next uint64) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("serverstore: store is closed")
	}
	if agentID == "" || next <= old || old > uint64(^uint64(0)>>1) || next > uint64(^uint64(0)>>1) {
		return false, errors.New("serverstore: invalid Agent incarnation transition")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE agents SET last_incarnation = ?
		WHERE id = ? AND last_incarnation = ?
	`, int64(next), agentID, int64(old))
	if err != nil {
		return false, fmt.Errorf("serverstore: advance Agent incarnation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("serverstore: inspect Agent incarnation update: %w", err)
	}
	return rows == 1, nil
}
