package serverstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Architecture section 15.1 and section 11 state what Server persistence must
// never contain: container/image/network/volume listings, container state,
// stats samples, logs, Compose file contents, and .env contents. Discovery
// results are not persisted either, beyond the project identity and applied
// fingerprints. The only bulk payload the design allows is the 64 KiB
// operation output tail.
//
// This audit locks the persisted surface so that a new place to mirror Docker
// or filesystem state cannot appear without a deliberate change here.

// persistedColumns is the complete Server persistence surface. Adding an entry
// requires confirming that the value is control-plane record data and not a
// mirror of Docker state, file contents, log text, or metric history.
var persistedColumns = map[string][]string{
	"agent_coverage_claims": {
		"id", "agent_id", "coverage_revision", "claim_type", "incarnation",
		"from_seq", "until_seq", "reason", "precision", "reported_at",
	},
	"agent_cursors": {
		"audit_archive_id", "agent_id", "next_incarnation", "next_seq",
		"acked_incarnation", "acked_seq", "coverage_revision_seen", "updated_at",
	},
	"agents": {
		"id", "display_name", "first_seen_at", "last_seen_at", "metadata_json",
		"capabilities_json", "retired_at", "last_incarnation",
		"projects_scanned_at", "project_scan_status_json",
	},
	"audit_events": {
		"id", "agent_id", "incarnation", "seq", "occurred_at", "kind", "actor",
		"project_uid", "operation_id", "metadata_json",
	},
	"backup_index": {
		"id", "agent_id", "project_uid", "kind", "created_at", "size_bytes",
		"storage_path", "manifest_sha256", "flags_json",
	},
	"join_tokens": {
		"id", "hash", "created_at", "expires_at", "consumed_at", "revoked_at",
	},
	"operations": {
		"id", "agent_id", "project_uid", "kind", "status", "phase", "revision",
		"actor", "requested_at", "started_at", "finished_at", "summary_json",
		"output_tail", "output_truncated",
	},
	"projects": {
		"project_uid", "agent_id", "working_dir", "name",
		"applied_fingerprints_json", "flags_json", "updated_at",
	},
	"schema_migrations": {"version", "applied_at"},
	"server_archive_coverage": {
		"id", "audit_archive_id", "agent_id", "entry_type", "from_incarnation",
		"from_seq", "until_incarnation", "until_seq", "source", "precision",
		"effective", "established_at", "resolved_at", "reason",
	},
	"settings": {"key", "value_json", "updated_at"},
}

// mirroringSubstrings name the state categories the architecture forbids in
// Server persistence. A column whose name contains one of these is either a
// mirror or named misleadingly; both need review.
var mirroringSubstrings = []string{
	"container", "image", "volume", "network", "stats", "metric",
	"cpu", "memory", "log_", "_log", "logs", "content", "env_", "_env",
	"dockerfile", "compose_file", "history", "sample",
}

func TestServerPersistenceSurfaceIsLockedAndFreeOfDockerMirrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)

	rows, err := store.DB().QueryContext(ctx, `
		SELECT m.name, p.name, p.type
		FROM sqlite_schema m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		ORDER BY m.name, p.cid
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	got := map[string][]string{}
	types := map[string]string{}
	for rows.Next() {
		var table, column, columnType string
		if err := rows.Scan(&table, &column, &columnType); err != nil {
			t.Fatal(err)
		}
		got[table] = append(got[table], column)
		types[table+"."+column] = strings.ToUpper(columnType)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if diff := diffColumnSets(persistedColumns, got); diff != "" {
		t.Fatalf("Server persistence surface changed; confirm the new columns are not Docker/file/log/metric mirrors, then update persistedColumns:\n%s", diff)
	}

	for table, columns := range got {
		for _, column := range columns {
			lowered := strings.ToLower(column)
			for _, forbidden := range mirroringSubstrings {
				if strings.Contains(lowered, forbidden) {
					t.Errorf("column %s.%s contains forbidden mirroring term %q", table, column, forbidden)
				}
			}
		}
	}
}

// TestOnlyBoundedBlobsArePersisted keeps bulk payloads out of the Server
// database. join_tokens.hash is a fixed 32-byte digest and operations
// .output_tail is the architecture's explicitly bounded 64 KiB tail; no other
// column may hold opaque bytes.
func TestOnlyBoundedBlobsArePersisted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t, ctx)

	rows, err := store.DB().QueryContext(ctx, `
		SELECT m.name || '.' || p.name
		FROM sqlite_schema m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		  AND UPPER(p.type) = 'BLOB'
		ORDER BY 1
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var blobs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []string{"join_tokens.hash", "operations.output_tail"}
	if strings.Join(blobs, ",") != strings.Join(want, ",") {
		t.Fatalf("BLOB columns = %v, want %v", blobs, want)
	}
}

func diffColumnSets(want, got map[string][]string) string {
	var report []string
	for table, wantColumns := range want {
		gotColumns, ok := got[table]
		if !ok {
			report = append(report, fmt.Sprintf("  missing table %s", table))
			continue
		}
		report = append(report, diffColumns(table, wantColumns, gotColumns)...)
	}
	for table := range got {
		if _, ok := want[table]; !ok {
			report = append(report, fmt.Sprintf("  unexpected table %s", table))
		}
	}
	sort.Strings(report)
	return strings.Join(report, "\n")
}

func diffColumns(table string, want, got []string) []string {
	wantSet := map[string]bool{}
	for _, column := range want {
		wantSet[column] = true
	}
	gotSet := map[string]bool{}
	for _, column := range got {
		gotSet[column] = true
	}
	var report []string
	for _, column := range got {
		if !wantSet[column] {
			report = append(report, fmt.Sprintf("  unexpected column %s.%s", table, column))
		}
	}
	for _, column := range want {
		if !gotSet[column] {
			report = append(report, fmt.Sprintf("  missing column %s.%s", table, column))
		}
	}
	return report
}
