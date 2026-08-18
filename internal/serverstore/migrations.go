package serverstore

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{version: 1, sql: schemaV1},
	{version: 2, sql: schemaV2},
	{version: 3, sql: schemaV3},
	{version: 4, sql: schemaV4},
	{version: 5, sql: schemaV5},
	{version: 6, sql: schemaV6},
}

func migrate(ctx context.Context, db *sql.DB) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("serverstore: acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("serverstore: begin migration: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if _, err = conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		return fmt.Errorf("serverstore: create migration ledger: %w", err)
	}

	var ledgerVersion int
	if err = conn.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations",
	).Scan(&ledgerVersion); err != nil {
		return fmt.Errorf("serverstore: read migration ledger: %w", err)
	}

	var userVersion int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("serverstore: read user_version: %w", err)
	}
	if userVersion != ledgerVersion {
		return fmt.Errorf(
			"serverstore: schema version markers disagree (user_version=%d, ledger=%d)",
			userVersion, ledgerVersion,
		)
	}
	if userVersion > CurrentSchemaVersion {
		return fmt.Errorf(
			"serverstore: database schema %d is newer than supported schema %d",
			userVersion, CurrentSchemaVersion,
		)
	}

	for _, migration := range migrations {
		if migration.version <= userVersion {
			continue
		}
		if migration.version != userVersion+1 {
			return fmt.Errorf(
				"serverstore: migration gap after version %d (next is %d)",
				userVersion, migration.version,
			)
		}
		if _, err = conn.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("serverstore: apply migration %d: %w", migration.version, err)
		}
		if _, err = conn.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))",
			migration.version,
		); err != nil {
			return fmt.Errorf("serverstore: record migration %d: %w", migration.version, err)
		}
		userVersion = migration.version
	}

	if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil {
		return fmt.Errorf("serverstore: update user_version: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("serverstore: commit migration: %w", err)
	}
	return nil
}

const schemaV1 = `
CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    capabilities_json TEXT NOT NULL DEFAULT '{}',
    retired_at TEXT
) STRICT;

CREATE TRIGGER agents_delete_forbidden
BEFORE DELETE ON agents
BEGIN
    SELECT RAISE(ABORT, 'agents must be retired, not deleted');
END;

CREATE TRIGGER agents_id_update_forbidden
BEFORE UPDATE OF id ON agents
BEGIN
    SELECT RAISE(ABORT, 'agent identity is immutable');
END;

CREATE TABLE join_tokens (
    id TEXT PRIMARY KEY,
    hash BLOB NOT NULL UNIQUE CHECK(length(hash) = 32),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT,
    revoked_at TEXT
) STRICT;

CREATE TABLE operations (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    project_uid TEXT,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    phase TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
    actor TEXT,
    requested_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    summary_json TEXT NOT NULL DEFAULT '{}',
    output_tail BLOB,
    output_truncated INTEGER NOT NULL DEFAULT 0 CHECK(output_truncated IN (0, 1)),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY,
    agent_id TEXT NOT NULL,
    incarnation INTEGER NOT NULL CHECK(incarnation >= 1),
    seq INTEGER NOT NULL CHECK(seq >= 1),
    occurred_at TEXT NOT NULL,
    kind TEXT NOT NULL,
    actor TEXT,
    project_uid TEXT,
    operation_id TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    UNIQUE(agent_id, incarnation, seq),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE agent_coverage_claims (
    id INTEGER PRIMARY KEY,
    agent_id TEXT NOT NULL,
    coverage_revision INTEGER NOT NULL CHECK(coverage_revision >= 0),
    claim_type TEXT NOT NULL CHECK(claim_type IN ('GAP', 'COVERAGE_UNKNOWN')),
    incarnation INTEGER NOT NULL CHECK(incarnation >= 1),
    from_seq INTEGER,
    until_seq INTEGER,
    reason TEXT NOT NULL,
    precision TEXT NOT NULL CHECK(precision IN ('exact', 'coalesced', 'unknown')),
    reported_at TEXT NOT NULL,
    CHECK(
        (claim_type = 'GAP' AND from_seq IS NOT NULL AND until_seq IS NOT NULL AND from_seq < until_seq)
        OR (claim_type = 'COVERAGE_UNKNOWN' AND from_seq IS NULL AND until_seq IS NULL)
    ),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE server_archive_coverage (
    id INTEGER PRIMARY KEY,
    audit_archive_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    entry_type TEXT NOT NULL CHECK(entry_type IN ('GAP', 'LOWER_BOUND', 'REGRESSION')),
    from_incarnation INTEGER,
    from_seq INTEGER,
    until_incarnation INTEGER,
    until_seq INTEGER,
    source TEXT NOT NULL,
    precision TEXT NOT NULL CHECK(precision IN ('exact', 'coalesced', 'unknown')),
    effective INTEGER NOT NULL CHECK(effective IN (0, 1)),
    established_at TEXT NOT NULL,
    resolved_at TEXT,
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE agent_cursors (
    audit_archive_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    next_incarnation INTEGER,
    next_seq INTEGER,
    acked_incarnation INTEGER,
    acked_seq INTEGER,
    coverage_revision_seen INTEGER NOT NULL DEFAULT 0 CHECK(coverage_revision_seen >= 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY(audit_archive_id, agent_id),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE projects (
    project_uid TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    working_dir TEXT NOT NULL,
    name TEXT NOT NULL,
    applied_fingerprints_json TEXT NOT NULL DEFAULT '[]',
    flags_json TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL,
    UNIQUE(agent_id, working_dir),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE backup_index (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    project_uid TEXT,
    kind TEXT NOT NULL,
    created_at TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    storage_path TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    flags_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX operations_agent_requested_idx
    ON operations(agent_id, requested_at DESC);
CREATE INDEX audit_events_agent_time_idx
    ON audit_events(agent_id, occurred_at DESC);
CREATE INDEX agent_coverage_claims_revision_idx
    ON agent_coverage_claims(agent_id, coverage_revision);
CREATE INDEX server_archive_coverage_lookup_idx
    ON server_archive_coverage(audit_archive_id, agent_id, effective);
CREATE INDEX projects_agent_name_idx
    ON projects(agent_id, name);
CREATE INDEX backup_index_project_created_idx
    ON backup_index(agent_id, project_uid, created_at DESC);
`

const schemaV2 = `
ALTER TABLE server_archive_coverage ADD COLUMN reason TEXT
    CHECK(reason IS NULL OR reason IN (
        'SERVER_NEVER_HAD',
        'NEW_AUDIT_ARCHIVE',
        'SERVER_DATABASE_REINITIALIZED'
    ));

CREATE TRIGGER server_archive_coverage_lower_bound_reason_insert
BEFORE INSERT ON server_archive_coverage
WHEN NEW.entry_type = 'LOWER_BOUND' AND NEW.reason IS NULL
BEGIN
    SELECT RAISE(ABORT, 'SERVER_COVERAGE_START reason is required');
END;

CREATE TRIGGER server_archive_coverage_lower_bound_reason_update
BEFORE UPDATE ON server_archive_coverage
WHEN NEW.entry_type = 'LOWER_BOUND' AND NEW.reason IS NULL
BEGIN
    SELECT RAISE(ABORT, 'SERVER_COVERAGE_START reason is required');
END;

-- A v1 database could technically contain a LOWER_BOUND for which the cause
-- was never stored. Do not invent CORE audit evidence during migration. This
-- validation deliberately aborts and rolls back v2 in that case.
UPDATE server_archive_coverage
SET reason = reason
WHERE entry_type = 'LOWER_BOUND';
`

// last_incarnation is a durable session-replay watermark, not a live Docker
// state mirror. The Reverse gRPC registry advances it atomically before a new
// Agent session becomes visible.
const schemaV3 = `
ALTER TABLE agents ADD COLUMN last_incarnation INTEGER NOT NULL DEFAULT 0
    CHECK(last_incarnation >= 0);
`

// SQLite cannot widen an ALTER-added column CHECK in place. Rebuild only the
// Coverage Ledger table so v4 can represent the frozen architecture's Server
// retention and cursor-regression reasons without weakening the lower-bound
// reason contract.
const schemaV4 = `
DROP TRIGGER server_archive_coverage_lower_bound_reason_insert;
DROP TRIGGER server_archive_coverage_lower_bound_reason_update;
DROP INDEX server_archive_coverage_lookup_idx;

ALTER TABLE server_archive_coverage RENAME TO server_archive_coverage_v3;

CREATE TABLE server_archive_coverage (
    id INTEGER PRIMARY KEY,
    audit_archive_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    entry_type TEXT NOT NULL CHECK(entry_type IN ('GAP', 'LOWER_BOUND', 'REGRESSION')),
    from_incarnation INTEGER,
    from_seq INTEGER,
    until_incarnation INTEGER,
    until_seq INTEGER,
    source TEXT NOT NULL,
    precision TEXT NOT NULL CHECK(precision IN ('exact', 'coalesced', 'unknown')),
    effective INTEGER NOT NULL CHECK(effective IN (0, 1)),
    established_at TEXT NOT NULL,
    resolved_at TEXT,
    reason TEXT CHECK(reason IS NULL OR reason IN (
        'SERVER_NEVER_HAD',
        'NEW_AUDIT_ARCHIVE',
        'SERVER_DATABASE_REINITIALIZED',
        'SERVER_RETENTION_APPLIED',
        'QUOTA_PRESSURE_BEFORE_AGENT_ACK',
        'DATABASE_RESTORE',
        'ARCHIVE_ROLLBACK',
        'CURSOR_METADATA_LOSS',
        'UNKNOWN'
    )),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

INSERT INTO server_archive_coverage(
    id, audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
    until_incarnation, until_seq, source, precision, effective,
    established_at, resolved_at, reason
)
SELECT id, audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
       until_incarnation, until_seq, source, precision, effective,
       established_at, resolved_at, reason
FROM server_archive_coverage_v3;

DROP TABLE server_archive_coverage_v3;

CREATE INDEX server_archive_coverage_lookup_idx
    ON server_archive_coverage(audit_archive_id, agent_id, effective);

CREATE TRIGGER server_archive_coverage_lower_bound_reason_insert
BEFORE INSERT ON server_archive_coverage
WHEN NEW.entry_type = 'LOWER_BOUND' AND (
    NEW.reason IS NULL OR NEW.reason NOT IN (
        'SERVER_NEVER_HAD', 'NEW_AUDIT_ARCHIVE', 'SERVER_DATABASE_REINITIALIZED'
    )
)
BEGIN
    SELECT RAISE(ABORT, 'SERVER_COVERAGE_START reason is required');
END;

CREATE TRIGGER server_archive_coverage_lower_bound_reason_update
BEFORE UPDATE ON server_archive_coverage
WHEN NEW.entry_type = 'LOWER_BOUND' AND (
    NEW.reason IS NULL OR NEW.reason NOT IN (
        'SERVER_NEVER_HAD', 'NEW_AUDIT_ARCHIVE', 'SERVER_DATABASE_REINITIALIZED'
    )
)
BEGIN
    SELECT RAISE(ABORT, 'SERVER_COVERAGE_START reason is required');
END;

CREATE TRIGGER server_archive_coverage_retention_reason_insert
BEFORE INSERT ON server_archive_coverage
WHEN NEW.source = 'SERVER_RETENTION' AND (
    NEW.entry_type != 'GAP' OR NEW.reason IS NULL OR NEW.reason NOT IN (
        'SERVER_RETENTION_APPLIED', 'QUOTA_PRESSURE_BEFORE_AGENT_ACK'
    )
)
BEGIN
    SELECT RAISE(ABORT, 'SERVER_RETENTION reason is invalid');
END;

CREATE TRIGGER server_archive_coverage_retention_reason_update
BEFORE UPDATE ON server_archive_coverage
WHEN NEW.source = 'SERVER_RETENTION' AND (
    NEW.entry_type != 'GAP' OR NEW.reason IS NULL OR NEW.reason NOT IN (
        'SERVER_RETENTION_APPLIED', 'QUOTA_PRESSURE_BEFORE_AGENT_ACK'
    )
)
BEGIN
    SELECT RAISE(ABORT, 'SERVER_RETENTION reason is invalid');
END;
`

// Project discovery is an Agent-owned observation, but the Server needs one
// per-Agent watermark even when a complete scan reports zero projects. Without
// it an older response could resurrect or overwrite newer mirror state.
const schemaV5 = `
ALTER TABLE agents ADD COLUMN projects_scanned_at TEXT
    CHECK(projects_scanned_at IS NULL OR (
        length(projects_scanned_at) = 30 AND
        substr(projects_scanned_at, 11, 1) = 'T' AND
        substr(projects_scanned_at, 20, 1) = '.' AND
        substr(projects_scanned_at, 30, 1) = 'Z'
    ));
ALTER TABLE agents ADD COLUMN project_scan_status_json TEXT NOT NULL DEFAULT '{}'
    CHECK(json_valid(project_scan_status_json) AND json_type(project_scan_status_json) = 'object' AND (
        (projects_scanned_at IS NULL AND project_scan_status_json = '{}') OR
        (projects_scanned_at IS NOT NULL AND project_scan_status_json != '{}')
    ));
`

// Recovery can learn an Agent-authoritative operation that the Server never
// finished recording before a crash. The recovery contract does not include
// the original request time, so keep that fact unknown instead of inventing a
// timestamp. All pre-v6 rows retain their existing requested_at value.
const schemaV6 = `
DROP INDEX operations_agent_requested_idx;
ALTER TABLE operations RENAME TO operations_v5;

CREATE TABLE operations (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    project_uid TEXT,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    phase TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
    actor TEXT,
    requested_at TEXT,
    started_at TEXT,
    finished_at TEXT,
    summary_json TEXT NOT NULL DEFAULT '{}',
    output_tail BLOB,
    output_truncated INTEGER NOT NULL DEFAULT 0 CHECK(output_truncated IN (0, 1)),
    FOREIGN KEY(agent_id) REFERENCES agents(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

INSERT INTO operations(
    id, agent_id, project_uid, kind, status, phase, revision, actor,
    requested_at, started_at, finished_at, summary_json, output_tail, output_truncated
)
SELECT id, agent_id, project_uid, kind, status, phase, revision, actor,
       requested_at, started_at, finished_at, summary_json, output_tail, output_truncated
FROM operations_v5;

DROP TABLE operations_v5;
CREATE INDEX operations_agent_requested_idx
    ON operations(agent_id, requested_at DESC);
`
