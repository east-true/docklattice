package auditwal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimAbandonedTempForDiskPressureCountsAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	wal := openTestWAL(t, dir, 1, testOptions())
	temp := filepath.Join(dir, ".coverage-state-orphan.tmp")
	if err := os.WriteFile(temp, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	freed, err := wal.ReclaimAbandonedTempForDiskPressure(context.Background(), 5)
	if err != nil || freed != 5 {
		t.Fatalf("freed=%d err=%v", freed, err)
	}
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		t.Fatalf("temp still exists: %v", err)
	}

	target := filepath.Join(t.TempDir(), "manual-data")
	if err := os.WriteFile(target, []byte("survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".once-orphan.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.ReclaimAbandonedTempForDiskPressure(context.Background(), 1); err == nil {
		t.Fatal("symlink temp was accepted")
	}
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "survive" {
		t.Fatalf("target=%q err=%v", payload, err)
	}
}
