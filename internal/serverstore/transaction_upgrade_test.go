package serverstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
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

// TestBeginWriteSurvivesTheSameInterleaving is the contract the write paths
// need: a transaction that is going to write takes the write lock before it
// reads, so a competing writer waits its turn instead of invalidating it.
func TestBeginWriteSurvivesTheSameInterleaving(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openScratchStore(ctx, t)

	upgrading, err := store.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer upgrading.Rollback()
	var seen int
	if err := upgrading.QueryRowContext(ctx, `SELECT count(*) FROM contention`).Scan(&seen); err != nil {
		t.Fatal(err)
	}

	// The competing writer now has to wait, so it is run concurrently and the
	// holder commits while it waits.
	competingErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		competing, beginErr := store.BeginWrite(ctx)
		if beginErr != nil {
			competingErr <- beginErr
			return
		}
		if _, execErr := competing.ExecContext(ctx, `INSERT INTO contention(id, note) VALUES(2, 'competing')`); execErr != nil {
			_ = competing.Rollback()
			competingErr <- execErr
			return
		}
		competingErr <- competing.Commit()
	}()

	if _, err := upgrading.ExecContext(ctx, `UPDATE contention SET note = 'upgraded' WHERE id = 1`); err != nil {
		t.Fatalf("write transaction upgrade error = %v, want success", err)
	}
	if err := upgrading.Commit(); err != nil {
		t.Fatalf("write transaction commit error = %v", err)
	}
	wg.Wait()
	if err := <-competingErr; err != nil {
		t.Fatalf("competing write transaction error = %v, want it to wait and succeed", err)
	}

	var notes int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM contention`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 2 {
		t.Fatalf("rows after both writers = %d, want 2", notes)
	}
}

// TestBeginWriteSerializesManyWriters is the burst shape the abuse matrix
// produces: many concurrent write transactions, each reading before it writes.
// Every one of them must complete rather than one of them being refused.
func TestBeginWriteSerializesManyWriters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openScratchStore(ctx, t)

	const writers = 40
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tx, err := store.BeginWrite(ctx)
			if err != nil {
				errs <- err
				return
			}
			var seen int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contention`).Scan(&seen); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO contention(id, note) VALUES(?, 'burst')`, id+100); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write transaction error = %v, want every writer to complete", err)
		}
	}

	var rows int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM contention`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if want := writers + 1; rows != want {
		t.Fatalf("rows after the burst = %d, want %d", rows, want)
	}
}

// TestBusyIsReportedAsTransient keeps genuine writer contention distinguishable
// from a corrupt or unexpected database error. A caller that waited out
// busy_timeout must be able to say so without inventing a new error framework.
func TestBusyIsReportedAsTransient(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openScratchStore(ctx, t)

	holder, err := store.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.ExecContext(ctx, `INSERT INTO contention(id, note) VALUES(2, 'held')`); err != nil {
		t.Fatal(err)
	}

	// A short deadline stands in for the busy timeout expiring.
	waiting, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, err = store.BeginWrite(waiting)
	if err == nil {
		t.Fatal("a second write transaction started while the write lock was held")
	}
	if !errors.Is(err, ErrBusy) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contention error = %v, want ErrBusy or a deadline", err)
	}
	if errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("contention error = %v, want a busy or deadline error", err)
	}
}
