package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskPressureSnapshotTiersNeverDeleteManualAndProtectNewestAutomatic(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "services: {}\n", 0o600)
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	manual := createBackup(t, manager, project, TriggerManual, "manual", base, "compose.yaml")
	var newest Backup
	for index := 0; index < AutomaticSnapshotRetention+3; index++ {
		newest = createBackup(t, manager, project, TriggerPreWrite,
			"auto-"+time.Duration(index).String(), base.Add(time.Duration(index+1)*time.Second), "compose.yaml")
	}

	freed, err := manager.ReclaimExcessAutomaticForDiskPressure(context.Background(), 1<<60, AutomaticSnapshotRetention)
	if err != nil || freed <= 0 {
		t.Fatalf("excess reclaim freed=%d err=%v", freed, err)
	}
	listed, err := manager.List(context.Background(), project.UID)
	if err != nil || len(listed) != AutomaticSnapshotRetention+1 {
		t.Fatalf("after excess: count=%d err=%v", len(listed), err)
	}

	freed, err = manager.ReclaimOldAutomaticForDiskPressure(context.Background(), 1<<60)
	if err != nil || freed <= 0 {
		t.Fatalf("old reclaim freed=%d err=%v", freed, err)
	}
	listed, err = manager.List(context.Background(), project.UID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("after old: entries=%+v err=%v", listed, err)
	}
	foundManual, foundNewest := false, false
	for _, item := range listed {
		foundManual = foundManual || item.BackupID == manual.Manifest.BackupID
		foundNewest = foundNewest || item.BackupID == newest.Manifest.BackupID
	}
	if !foundManual || !foundNewest {
		t.Fatalf("protected backups missing: manual=%v newest=%v entries=%+v", foundManual, foundNewest, listed)
	}
}

func TestReclaimAbandonedTempCountsBytesAndRejectsSymlink(t *testing.T) {
	manager, project := newTestManager(t)
	projectDir := filepath.Join(manager.backupDir, project.UID)
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := newBackupID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(projectDir, "."+id+".tmp")
	if err := os.Mkdir(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(tempDir, "partial"), "1234567", 0o600)
	freed, err := manager.ReclaimAbandonedTempForDiskPressure(context.Background(), 7)
	if err != nil || freed != 7 {
		t.Fatalf("freed=%d err=%v", freed, err)
	}
	if _, err := os.Lstat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned directory still exists: %v", err)
	}

	target := filepath.Join(t.TempDir(), "must-survive")
	mustWrite(t, target, "manual data", 0o600)
	link := filepath.Join(manager.journalDir, ".orphan.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReclaimAbandonedTempForDiskPressure(context.Background(), 1); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink reclaim error=%v", err)
	}
	if got := readString(t, target); got != "manual data" {
		t.Fatalf("symlink target changed: %q", got)
	}
}
