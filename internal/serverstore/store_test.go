package serverstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesOwnerOnlyDatabaseAndRejectsExposedFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %04o, want 0600", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("Open exposed database error = %v", err)
	}
}

func TestOpenCreatesSchemaAndConfiguresSQLite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)

	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, CurrentSchemaVersion)
	}

	wantTables := []string{
		"agent_coverage_claims",
		"agent_cursors",
		"agents",
		"audit_events",
		"backup_index",
		"join_tokens",
		"operations",
		"projects",
		"schema_migrations",
		"server_archive_coverage",
		"settings",
	}
	rows, err := store.DB().QueryContext(ctx, `
		SELECT name
		FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var gotTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		gotTables = append(gotTables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotTables, ",") != strings.Join(wantTables, ",") {
		t.Fatalf("tables = %v, want %v", gotTables, wantTables)
	}

	assertPragmaInt(t, ctx, store.DB(), "foreign_keys", 1)
	assertPragmaInt(t, ctx, store.DB(), "busy_timeout", busyTimeoutMillis)

	var journalMode string
	if err := store.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want WAL", journalMode)
	}
}

func TestMigrationIsIdempotentAndSurvivesReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.DB().ExecContext(ctx,
		"INSERT INTO settings(key, value_json, updated_at) VALUES ('site_name', ?, ?)",
		`{"value":"lab"}`, "2026-08-15T00:00:00Z",
	); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, store.DB()); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	var value string
	if err := reopened.DB().QueryRowContext(ctx,
		"SELECT value_json FROM settings WHERE key = 'site_name'",
	).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != `{"value":"lab"}` {
		t.Fatalf("persisted setting = %q", value)
	}

	var migrationCount int
	if err := reopened.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
		CurrentSchemaVersion,
	).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration ledger count = %d, want 1", migrationCount)
	}
}

func TestV1DatabaseUpgradesSequentiallyToCurrent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	createV1Database(t, ctx, path, nil)

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, CurrentSchemaVersion)
	}
	var migrationCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != CurrentSchemaVersion {
		t.Fatalf("migration count = %d, want %d", migrationCount, CurrentSchemaVersion)
	}
	rows, err := store.DB().QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(versions, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("migration versions = %v, want [1 2 3 4 5 6]", versions)
	}
	columns := tableColumns(t, ctx, store.DB(), "server_archive_coverage")
	if !slices.Contains(columns, "reason") {
		t.Fatalf("server_archive_coverage columns = %v", columns)
	}
	agentColumns := tableColumns(t, ctx, store.DB(), "agents")
	if !slices.Contains(agentColumns, "projects_scanned_at") || !slices.Contains(agentColumns, "project_scan_status_json") {
		t.Fatalf("agents columns = %v", agentColumns)
	}
}

func TestIncarnationWatermarkIsDurableAtomicAndMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, ctx)
	insertAgent(t, ctx, store.DB(), "agent-1")

	if got, err := store.LoadIncarnation(ctx, "agent-1"); err != nil || got != 0 {
		t.Fatalf("initial watermark = %d, %v", got, err)
	}
	if swapped, err := store.CompareAndSwapIncarnation(ctx, "agent-1", 0, 4); err != nil || !swapped {
		t.Fatalf("CAS 0->4 = %v, %v", swapped, err)
	}
	if swapped, err := store.CompareAndSwapIncarnation(ctx, "agent-1", 0, 5); err != nil || swapped {
		t.Fatalf("stale CAS 0->5 = %v, %v", swapped, err)
	}
	if got, err := store.LoadIncarnation(ctx, "agent-1"); err != nil || got != 4 {
		t.Fatalf("final watermark = %d, %v", got, err)
	}
	if _, err := store.CompareAndSwapIncarnation(ctx, "agent-1", 4, 4); err == nil {
		t.Fatal("non-monotonic transition succeeded")
	}
	if _, err := store.LoadIncarnation(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing Agent error = %v", err)
	}
}

func TestProjectScanWatermarkSchemaRequiresFixedUTCAndObjectStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, ctx)
	insertAgent(t, ctx, store.DB(), "agent-1")
	validStatus := `{"scanned_at":"2026-08-15T00:00:00Z","truncated":false,"directories_seen":0}`
	if _, err := store.DB().ExecContext(ctx, `
		UPDATE agents SET projects_scanned_at = ?, project_scan_status_json = ? WHERE id = 'agent-1'
	`, "2026-08-15T00:00:00.000000000Z", validStatus); err != nil {
		t.Fatalf("valid project watermark: %v", err)
	}
	for _, update := range []struct{ watermark, status string }{
		{"2026-08-15T00:00:00Z", validStatus},
		{"2026-08-15T00:00:01.000000000Z", `[]`},
		{"2026-08-15T00:00:01.000000000Z", `{}`},
	} {
		if _, err := store.DB().ExecContext(ctx, `
			UPDATE agents SET projects_scanned_at = ?, project_scan_status_json = ? WHERE id = 'agent-1'
		`, update.watermark, update.status); err == nil {
			t.Fatalf("invalid project watermark/status accepted: %+v", update)
		}
	}
}

func TestOperationRecoveryMigrationPreservesKnownRequestTimesAndAllowsUnknown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	createV1Database(t, ctx, path, func(db *sql.DB) {
		insertAgent(t, ctx, db, "agent-1")
		if _, err := db.ExecContext(ctx, `
			INSERT INTO operations(id, agent_id, kind, status, phase, requested_at)
			VALUES('known', 'agent-1', 'docker.prune', 'running', 'EXECUTING', '2026-08-15T00:00:00Z')
		`); err != nil {
			t.Fatal(err)
		}
	})
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var known string
	if err := store.DB().QueryRowContext(ctx, `SELECT requested_at FROM operations WHERE id = 'known'`).Scan(&known); err != nil || known != "2026-08-15T00:00:00Z" {
		t.Fatalf("known requested_at = %q, %v", known, err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO operations(id, agent_id, kind, status, phase, requested_at)
		VALUES('recovered', 'agent-1', 'docker.prune', 'running', 'EXECUTING', NULL)
	`); err != nil {
		t.Fatalf("unknown recovered request time: %v", err)
	}
}

func TestV1MigrationDoesNotInventMissingCoverageStartReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	createV1Database(t, ctx, path, func(db *sql.DB) {
		insertAgent(t, ctx, db, "agent-1")
		if _, err := db.ExecContext(ctx, `
			INSERT INTO server_archive_coverage(
				audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
				source, precision, effective, established_at
			) VALUES ('archive-1', 'agent-1', 'LOWER_BOUND', 1, 1,
			          'SERVER_COVERAGE_START', 'exact', 0, '2026-08-15T00:00:00Z')
		`); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("Open v1 with reasonless lower bound error = %v", err)
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, migrationCount int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if version != 1 || migrationCount != 1 {
		t.Fatalf("failed migration left version=%d ledger=%d, want 1/1", version, migrationCount)
	}
	if slices.Contains(tableColumns(t, ctx, db, "server_archive_coverage"), "reason") {
		t.Fatal("failed v2 migration did not roll back ALTER TABLE")
	}
}

func TestFreshCoverageReasonConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, ctx)
	insertAgent(t, ctx, store.DB(), "agent-1")
	insert := func(reason any) error {
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO server_archive_coverage(
				audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
				source, precision, effective, established_at, reason
			) VALUES ('archive-1', 'agent-1', 'LOWER_BOUND', 1, 1,
			          'SERVER_COVERAGE_START', 'exact', 0, '2026-08-15T00:00:00Z', ?)
		`, reason)
		return err
	}
	if err := insert(nil); err == nil {
		t.Fatal("LOWER_BOUND without reason succeeded")
	}
	if err := insert("INFERRED_REASON"); err == nil {
		t.Fatal("unknown coverage-start reason succeeded")
	}
	if err := insert("NEW_AUDIT_ARCHIVE"); err != nil {
		t.Fatalf("defined coverage-start reason: %v", err)
	}

	insertRetention := func(reason string) error {
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO server_archive_coverage(
				audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
				until_incarnation, until_seq, source, precision, effective,
				established_at, reason
			) VALUES ('archive-1', 'agent-1', 'GAP', 1, 2, 1, 3,
			          'SERVER_RETENTION', 'exact', 1, '2026-08-15T00:00:00Z', ?)
		`, reason)
		return err
	}
	if err := insertRetention("SERVER_RETENTION_APPLIED"); err != nil {
		t.Fatalf("ACKed retention reason: %v", err)
	}
	if err := insertRetention("QUOTA_PRESSURE_BEFORE_AGENT_ACK"); err != nil {
		t.Fatalf("unACKed retention reason: %v", err)
	}
	if err := insertRetention("NEW_AUDIT_ARCHIVE"); err == nil {
		t.Fatal("coverage-start reason was accepted for Server retention")
	}
}

func TestAgentDeleteIsForbiddenAndAuditForeignKeyDoesNotCascade(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)
	insertAgent(t, ctx, store.DB(), "agent-1")
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO audit_events(
			agent_id, incarnation, seq, occurred_at, kind, metadata_json
		) VALUES ('agent-1', 1, 1, '2026-08-15T00:00:00Z', 'AGENT_REGISTERED', '{}')
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DB().ExecContext(ctx, "DELETE FROM agents WHERE id = 'agent-1'"); err == nil {
		t.Fatal("deleting an Agent succeeded; want retirement-only invariant")
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE agents SET id = 'agent-renamed' WHERE id = 'agent-1'"); err == nil {
		t.Fatal("updating stable Agent identity succeeded")
	}

	var count int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_events WHERE agent_id = 'agent-1'",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit event count after rejected delete = %d, want 1", count)
	}

	rows, err := store.DB().QueryContext(ctx, "PRAGMA foreign_key_list(audit_events)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var foundRestrict bool
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if table == "agents" && from == "agent_id" && strings.EqualFold(onDelete, "RESTRICT") && strings.EqualFold(onUpdate, "RESTRICT") {
			foundRestrict = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundRestrict {
		t.Fatal("audit_events.agent_id does not have ON DELETE RESTRICT")
	}
}

func TestAuditIdentityIsUnique(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)
	insertAgent(t, ctx, store.DB(), "agent-1")

	insert := func(incarnation, seq int) error {
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO audit_events(
				agent_id, incarnation, seq, occurred_at, kind, metadata_json
			) VALUES ('agent-1', ?, ?, '2026-08-15T00:00:00Z', 'TEST', '{}')
		`, incarnation, seq)
		return err
	}
	if err := insert(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := insert(1, 1); err == nil {
		t.Fatal("duplicate (agent_id, incarnation, seq) succeeded")
	}
	if err := insert(1, 2); err != nil {
		t.Fatalf("next sequence rejected: %v", err)
	}
	if err := insert(2, 1); err != nil {
		t.Fatalf("next incarnation rejected: %v", err)
	}
}

func TestJoinTokensStoreOnlyHashMaterial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)
	columns := tableColumns(t, ctx, store.DB(), "join_tokens")
	want := []string{"consumed_at", "created_at", "expires_at", "hash", "id", "revoked_at"}
	sort.Strings(columns)
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Fatalf("join_tokens columns = %v, want %v", columns, want)
	}

	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO join_tokens(id, hash, created_at, expires_at)
		VALUES ('join-1', zeroblob(32), '2026-08-15T00:00:00Z', '2026-08-16T00:00:00Z')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO join_tokens(id, hash, created_at, expires_at)
		VALUES ('join-2', zeroblob(32), '2026-08-15T00:00:00Z', '2026-08-16T00:00:00Z')
	`); err == nil {
		t.Fatal("duplicate join token hash succeeded")
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO join_tokens(id, hash, created_at, expires_at)
		VALUES ('join-short', X'01', '2026-08-15T00:00:00Z', '2026-08-16T00:00:00Z')
	`); err == nil {
		t.Fatal("non-SHA-256 join token hash succeeded")
	}
}

func openTestStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createV1Database(t *testing.T, ctx context.Context, path string, populate func(*sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, schemaV1); err != nil {
		t.Fatal(err)
	}
	if populate != nil {
		populate(db)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, applied_at) VALUES (1, '2026-08-15T00:00:00Z')",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func insertAgent(t *testing.T, ctx context.Context, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at)
		VALUES (?, ?, '2026-08-15T00:00:00Z', '2026-08-15T00:00:00Z')
	`, id, id); err != nil {
		t.Fatal(err)
	}
}

func assertPragmaInt(t *testing.T, ctx context.Context, db *sql.DB, pragma string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %d, want %d", pragma, got, want)
	}
}

func tableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func TestTouchAgentLastSeenIsMonotonicAndDoesNotReviveRetiredAgent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial := "2026-08-15T00:00:00.000000000Z"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at) VALUES ('agent-touch', 'Agent', ?, ?)
	`, initial, initial); err != nil {
		t.Fatal(err)
	}
	newer := time.Date(2026, 8, 15, 1, 2, 3, 4, time.UTC)
	if err := store.TouchAgentLastSeen(ctx, "agent-touch", newer); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAgentLastSeen(ctx, "agent-touch", newer.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := store.DB().QueryRowContext(ctx, `SELECT last_seen_at FROM agents WHERE id='agent-touch'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != newer.Format(serverDatabaseTimeFormat) {
		t.Fatalf("last_seen_at = %q", stored)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE agents SET retired_at=? WHERE id='agent-touch'`, stored); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAgentLastSeen(ctx, "agent-touch", newer.Add(time.Hour)); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("retired touch error = %v", err)
	}
	if err := store.TouchAgentLastSeen(ctx, "missing", newer); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing touch error = %v", err)
	}
}

func TestRestoreAuthenticatedAgentRecreatesTheRowButNeverRevivesRetirement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	observed := time.Date(2026, 8, 15, 1, 2, 3, 4, time.UTC)

	// A verified credential whose row is gone is exactly the Audit database
	// loss case, and the observation must land after the restore.
	if err := store.TouchAgentLastSeen(ctx, "agent-restore", observed); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("precondition touch error = %v", err)
	}
	if err := store.RestoreAuthenticatedAgent(ctx, "agent-restore", observed); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAgentLastSeen(ctx, "agent-restore", observed); err != nil {
		t.Fatalf("touch after restore = %v", err)
	}
	var displayName, firstSeen string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT display_name, first_seen_at FROM agents WHERE id='agent-restore'`).Scan(&displayName, &firstSeen); err != nil {
		t.Fatal(err)
	}
	if displayName != "agent-restore" {
		t.Fatalf("restored display_name = %q", displayName)
	}
	if firstSeen != observed.Format(serverDatabaseTimeFormat) {
		t.Fatalf("restored first_seen_at = %q", firstSeen)
	}

	// Restoring again must not disturb the existing row.
	if _, err := store.DB().ExecContext(ctx, `UPDATE agents SET display_name='named' WHERE id='agent-restore'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreAuthenticatedAgent(ctx, "agent-restore", observed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT display_name FROM agents WHERE id='agent-restore'`).Scan(&displayName); err != nil {
		t.Fatal(err)
	}
	if displayName != "named" {
		t.Fatalf("idempotent restore overwrote display_name: %q", displayName)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE agents SET retired_at=? WHERE id='agent-restore'`,
		observed.Format(serverDatabaseTimeFormat)); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreAuthenticatedAgent(ctx, "agent-restore", observed); !errors.Is(err, ErrAgentRetired) {
		t.Fatalf("retired restore error = %v", err)
	}
	if err := store.RestoreAuthenticatedAgent(ctx, "", observed); err == nil {
		t.Fatal("an empty Agent ID must be rejected")
	}
}
