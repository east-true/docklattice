//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenedRootFilesystemSpaceRemainsBoundAfterPathSwap(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "project")
	moved := filepath.Join(parent, "project-original")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	wantTotal, wantFree, err := openedRootFilesystemSpace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc", path); err != nil {
		t.Fatal(err)
	}
	gotTotal, gotFree, err := openedRootFilesystemSpace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if gotTotal != wantTotal || gotFree <= 0 || wantFree <= 0 {
		t.Fatalf("opened-root filesystem changed: before=%d/%d after=%d/%d", wantTotal, wantFree, gotTotal, gotFree)
	}
}
