package serverbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/identity"
)

func TestRecoveryIdentityStateLostWithDatabasePreservedFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	oldArchive := first.Archive
	credential, err := first.Identity.IssueCredential("agent-recovery", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(dir, "identity", "server-identity.json")
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}

	if components, err := Open(ctx, dir); components != nil || !errors.Is(err, ErrServerIdentityMismatch) {
		if components != nil {
			_ = components.Close()
		}
		t.Fatalf("identity loss with preserved DB = components %v, error %v", components != nil, err)
	}
	replacement, err := identity.Open(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ServerIdentityID() == oldArchive.ServerIdentityID {
		t.Fatal("identity loss recreated the previous Server identity")
	}
	if err := replacement.VerifyCredential(credential, credential.IssuedAt); !errors.Is(err, identity.ErrUnknownSigningKey) {
		t.Fatalf("old credential under replacement identity = %v", err)
	}
}

func TestRecoveryDatabaseLostWithIdentityPreservedCreatesEmptyHigherArchive(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	oldArchive := first.Archive
	credential, err := first.Identity.IssueCredential("agent-recovery", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Store.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at)
		VALUES ('agent-recovery', 'agent', '2026-08-15T00:00:00Z', '2026-08-15T00:00:00Z')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Store.DB().ExecContext(ctx, `
		INSERT INTO agent_cursors(
			audit_archive_id, agent_id, next_incarnation, next_seq,
			acked_incarnation, acked_seq, coverage_revision_seen, updated_at
		) VALUES (?, 'agent-recovery', 4, 10, 4, 9, 3, '2026-08-15T00:00:00Z')
	`, oldArchive.AuditArchiveID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Store.DB().ExecContext(ctx, `
		INSERT INTO server_archive_coverage(
			audit_archive_id, agent_id, entry_type, from_incarnation, from_seq,
			source, precision, effective, established_at, reason
		) VALUES (?, 'agent-recovery', 'LOWER_BOUND', 1, 1,
		          'SERVER_COVERAGE_START', 'exact', 0, '2026-08-15T00:00:00Z', 'SERVER_NEVER_HAD')
	`, oldArchive.AuditArchiveID); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "server.db")); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Archive.ServerIdentityID != oldArchive.ServerIdentityID ||
		second.Archive.Generation != oldArchive.Generation+1 ||
		second.Archive.AuditArchiveID == oldArchive.AuditArchiveID {
		t.Fatalf("replacement archive = %#v, old = %#v", second.Archive, oldArchive)
	}
	if err := second.Identity.VerifyCredential(credential, credential.IssuedAt); err != nil {
		t.Fatalf("DB loss invalidated identity-owned credential: %v", err)
	}
	for _, table := range []string{"agents", "agent_cursors", "server_archive_coverage"} {
		var count int
		if err := second.Store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("new archive inherited %s rows: %d", table, count)
		}
	}
}

func TestRecoveryBothServerStoresLostCreatesNewTrustDomain(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	oldArchive := first.Archive
	credential, err := first.Identity.IssueCredential("agent-recovery", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "server.db"),
		filepath.Join(dir, "identity", "server-identity.json"),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	second, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Archive.ServerIdentityID == oldArchive.ServerIdentityID || second.Archive.Generation != 1 ||
		second.Archive.AuditArchiveID == oldArchive.AuditArchiveID {
		t.Fatalf("replacement trust domain = %#v, old = %#v", second.Archive, oldArchive)
	}
	if err := second.Identity.VerifyCredential(credential, credential.IssuedAt); !errors.Is(err, identity.ErrUnknownSigningKey) {
		t.Fatalf("old credential under new trust domain = %v", err)
	}
}

func TestRecoveryRestoredOlderIdentityGenerationIsArchiveRollback(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(dir, "identity", "server-identity.json")
	oldIdentity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	oldArchive := first.Archive
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "server.db")); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Archive.Generation != oldArchive.Generation+1 {
		t.Fatalf("second archive = %#v, old = %#v", second.Archive, oldArchive)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityPath, oldIdentity, 0o600); err != nil {
		t.Fatal(err)
	}
	if components, err := Open(ctx, dir); components != nil || !errors.Is(err, ErrArchiveRollback) {
		if components != nil {
			_ = components.Close()
		}
		t.Fatalf("restored older identity generation = components %v, error %v", components != nil, err)
	}
}
