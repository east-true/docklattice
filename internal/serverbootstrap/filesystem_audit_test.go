package serverbootstrap

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Architecture section 15.1 keeps Docker state, Compose file contents, .env
// contents, log text, and metric history out of Server persistence. The
// database side of that promise is locked in
// internal/serverstore/persistence_audit_test.go; this test locks the
// filesystem side, so a future component cannot start writing mirrored state
// beside the database instead of inside it.
//
// SQLite may or may not have flushed its WAL and shared-memory sidecars when
// the tree is inspected, so those two names are optional. Everything else is
// an exact match.
func TestServerStateDirectoryHoldsOnlyIdentityAndDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stateDir := t.TempDir()

	components, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = components.Close() })

	required := map[string]bool{
		"identity":                      true,
		"identity/server-identity.json": true,
		"server.db":                     true,
	}
	optional := map[string]bool{
		"server.db-wal": true,
		"server.db-shm": true,
	}

	var found []string
	err = filepath.WalkDir(stateDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stateDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)

	for _, rel := range found {
		if !required[rel] && !optional[rel] {
			t.Errorf("unexpected Server state entry %q; Server persistence must not mirror Docker state, file contents, logs, or metric history", rel)
		}
	}
	for rel := range required {
		if !slicesContains(found, rel) {
			t.Errorf("missing required Server state entry %q (found %v)", rel, found)
		}
	}

	// The identity file is the only JSON the Server keeps on disk. It must
	// hold identity material, not a snapshot of hosts, projects, or Docker
	// objects.
	identityBytes, err := os.ReadFile(filepath.Join(stateDir, "identity", "server-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"container", "image", "volume", "network", "stats", "metric",
		"log", "compose", "services", "env",
	} {
		if strings.Contains(strings.ToLower(string(identityBytes)), forbidden) {
			t.Errorf("server-identity.json contains forbidden term %q: %s", forbidden, identityBytes)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}
