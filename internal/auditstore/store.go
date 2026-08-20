package auditstore

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

type runtimeKey struct {
	archive string
	agent   string
}

type runtimeObservation struct {
	lastIngestAt         time.Time
	firstUnackedIngestAt time.Time
	ingestedSinceACK     bool
	staleCoverageTotal   uint64
	ackRetryTotal        uint64
	blockedSince         time.Time
}

type Store struct {
	db      *sql.DB
	mu      sync.Mutex
	runtime map[runtimeKey]*runtimeObservation
}

func New(db *sql.DB) *Store {
	return &Store{db: db, runtime: make(map[runtimeKey]*runtimeObservation)}
}

type connectionTx struct {
	conn *sql.Conn
}

func (s *Store) withImmediate(ctx context.Context, fn func(*connectionTx) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(&connectionTx{conn: conn}); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	return nil
}

// reader is the query surface shared by both transaction kinds. Helpers that
// only read take this, so they can be called from either.
type reader interface {
	query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	row(ctx context.Context, query string, args ...any) *sql.Row
}

// readTx is a deferred transaction's handle. It deliberately has no exec: a
// deferred transaction that later tries to write is refused outright, without
// waiting on busy_timeout, which is the contention defect the write path was
// changed for. Rather than leave that to a comment and a careful reader, a body
// running under withRead is handed something that cannot write at all.
type readTx struct{ conn *sql.Conn }

func (tx *readTx) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}
func (tx *readTx) row(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

// withRead runs a read-only body inside one deferred transaction, so several
// queries see a single consistent snapshot without taking the write lock.
func (s *Store) withRead(ctx context.Context, fn func(reader) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}()
	return fn(&readTx{conn: conn})
}

func (tx *connectionTx) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}
func (tx *connectionTx) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}
func (tx *connectionTx) row(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (s *Store) runtimeFor(archive, agent string) *runtimeObservation {
	key := runtimeKey{archive, agent}
	state := s.runtime[key]
	if state == nil {
		state = &runtimeObservation{}
		s.runtime[key] = state
	}
	return state
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("auditstore: invalid persisted timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func compareCursor(left, right Cursor) int {
	if left.Incarnation < right.Incarnation {
		return -1
	}
	if left.Incarnation > right.Incarnation {
		return 1
	}
	if left.Seq < right.Seq {
		return -1
	}
	if left.Seq > right.Seq {
		return 1
	}
	return 0
}

func cloneCursor(cursor *Cursor) *Cursor {
	if cursor == nil {
		return nil
	}
	copy := *cursor
	return &copy
}
