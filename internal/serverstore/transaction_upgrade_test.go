package serverstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchTable gives these tests a table of their own so they exercise the
// transaction boundary rather than any particular schema object.
func scratchTable(ctx context.Context, t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE contention(id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO contention(id, note) VALUES(1, 'seed')`); err != nil {
		t.Fatal(err)
	}
}

func openScratchStore(ctx context.Context, t *testing.T) *Store {
	t.Helper()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scratchTable(ctx, t, store)
	return store
}

func isBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database is locked")
}

// TestDeferredTransactionUpgradeIsRefusedWithoutWaiting pins the defect that
// made the Server answer HTTP 500 under an operation burst.
//
// A transaction begun with database/sql's default options is DEFERRED: it
// takes no lock until its first statement, and a read is its first statement.
// If another connection commits a write while that read snapshot is open, the
// later upgrade to a write cannot succeed - the reader's snapshot is already
// stale - so SQLite refuses it with SQLITE_BUSY *and does not run the busy
// handler*, because waiting could never help. busy_timeout is therefore no
// defence at all for this shape, which is why the failure looked intermittent
// rather than slow.
func TestDeferredTransactionUpgradeIsRefusedWithoutWaiting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openScratchStore(ctx, t)

	reader, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	// The path under test: read, decide, then write, all in one transaction.
	upgrading, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upgrading.Rollback()
	var seen int
	if err := upgrading.QueryRowContext(ctx, `SELECT count(*) FROM contention`).Scan(&seen); err != nil {
		t.Fatal(err)
	}

	// A second writer commits while that snapshot is open.
	competing, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := competing.ExecContext(ctx, `INSERT INTO contention(id, note) VALUES(2, 'competing')`); err != nil {
		t.Fatal(err)
	}
	if err := competing.Commit(); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = upgrading.ExecContext(ctx, `UPDATE contention SET note = 'upgraded' WHERE id = 1`)
	elapsed := time.Since(started)

	if !isBusy(err) {
		t.Fatalf("deferred upgrade error = %v, want a locked-database refusal", err)
	}
	// The refusal is immediate. If busy_timeout applied, this would have taken
	// at least busyTimeoutMillis, and the fix would be a longer timeout rather
	// than a different transaction boundary.
	if elapsed >= time.Duration(busyTimeoutMillis)*time.Millisecond {
		t.Fatalf("deferred upgrade waited %s; SQLITE_BUSY on upgrade must not consult busy_timeout", elapsed)
	}
}
