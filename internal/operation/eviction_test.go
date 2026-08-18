package operation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimExpiredForDiskPressureDeletesDurableResultAndOutputOnly(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)}
	config := DefaultConfig()
	config.Clock = clock
	config.Journal = journal
	config.ResultRetention = time.Hour
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	expired := create(t, engine, "expired", TypeDiscoveryRescan, "")
	if _, err := expired.WriteOutput([]byte("retained output")); err != nil {
		t.Fatal(err)
	}
	if err := expired.FlushOutputTail(); err != nil {
		t.Fatal(err)
	}
	fail(t, expired)
	active := create(t, engine, "active", TypeDiscoveryRescan, "")
	clock.Advance(2 * time.Hour)

	var expected int64
	for _, name := range []string{operationJournalName("expired"), operationOutputName("expired")} {
		info, err := os.Lstat(filepath.Join(journal.directory, name))
		if err != nil {
			t.Fatal(err)
		}
		expected += info.Size()
	}
	freed, err := engine.ReclaimExpiredForDiskPressure(context.Background(), 1<<20)
	if err != nil || freed != expected {
		t.Fatalf("freed=%d want=%d err=%v", freed, expected, err)
	}
	if _, ok := engine.items[expired.spec.OperationID]; ok {
		t.Fatal("expired result remains indexed")
	}
	if _, ok := engine.items[active.spec.OperationID]; !ok {
		t.Fatal("active operation was evicted")
	}
	if _, err := os.Lstat(filepath.Join(journal.directory, operationJournalName("active"))); err != nil {
		t.Fatalf("active journal removed: %v", err)
	}
}

func TestJournalAbandonedTempReclaimIsSymlinkSafe(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "manual-data")
	if err := os.WriteFile(target, []byte("survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(journal.directory, ".orphan.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ReclaimAbandonedTempForDiskPressure(context.Background(), 1); err == nil {
		t.Fatal("symlink temp was accepted")
	}
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "survive" {
		t.Fatalf("target=%q err=%v", payload, err)
	}
}
