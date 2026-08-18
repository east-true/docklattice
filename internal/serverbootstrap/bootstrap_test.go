package serverbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesStableIndependentStores(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	firstArchive := first.Archive
	if firstArchive.Generation != 1 || firstArchive.AuditArchiveID == "" {
		t.Fatalf("first archive = %#v", firstArchive)
	}
	if first.Identity.ArchiveGeneration() != firstArchive.Generation {
		t.Fatal("identity generation and archive differ")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Archive != firstArchive {
		t.Fatalf("archive changed across reopen: got %#v want %#v", second.Archive, firstArchive)
	}
	if second.Identity.ServerIdentityID() != firstArchive.ServerIdentityID {
		t.Fatal("Server Identity changed across reopen")
	}
	for _, path := range []string{
		filepath.Join(dir, "server.db"),
		filepath.Join(dir, "identity", "server-identity.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %04o", path, info.Mode().Perm())
		}
	}
}

func TestOperationalDatabaseLossCreatesHigherArchive(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	old := first.Archive
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
	if second.Archive.ServerIdentityID != old.ServerIdentityID ||
		second.Archive.Generation <= old.Generation ||
		second.Archive.AuditArchiveID == old.AuditArchiveID {
		t.Fatalf("replacement archive = %#v, old = %#v", second.Archive, old)
	}
}

func TestArchiveAheadOfIdentityIsRejected(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "server")
	components, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	ahead := components.Archive
	ahead.Generation++
	payload, err := json.Marshal(ahead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := components.Store.DB().ExecContext(ctx,
		"UPDATE settings SET value_json = ? WHERE key = ?", string(payload), archiveSettingKey,
	); err != nil {
		t.Fatal(err)
	}
	if err := components.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); !errors.Is(err, ErrArchiveRollback) {
		t.Fatalf("Open ahead archive error = %v", err)
	}
}

func TestStateDirectorySafety(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, "relative"); err == nil {
		t.Fatal("relative state directory accepted")
	}
	dir := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("group/other-writable state directory accepted")
	}
}
