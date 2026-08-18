//go:build linux

package safefile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimAbandonedStagingIsExactAndSymlinkSafe(t *testing.T) {
	dir := t.TempDir()
	root, err := OpenRoot(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stage := filepath.Join(dir, ".dockpilot-stage-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := os.WriteFile(stage, []byte("1234567"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreStage := filepath.Join(dir, ".dockpilot-restore-operation-000.tmp")
	if err := os.WriteFile(restoreStage, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	freed, err := root.ReclaimAbandonedStagingForDiskPressure(context.Background(), 7)
	if err != nil || freed != 7 {
		t.Fatalf("freed=%d err=%v", freed, err)
	}
	if _, err := os.Lstat(restoreStage); err != nil {
		t.Fatalf("restore staging was selected: %v", err)
	}

	target := filepath.Join(t.TempDir(), "manual")
	if err := os.WriteFile(target, []byte("survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".dockpilot-stage-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReclaimAbandonedStagingForDiskPressure(context.Background(), 1); err == nil {
		t.Fatal("staging symlink was accepted")
	}
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "survive" {
		t.Fatalf("target=%q err=%v", payload, err)
	}
}

func TestReclaimAbandonedStagingWaitsForWriteInAnotherRoot(t *testing.T) {
	dir := t.TempDir()
	original := []byte("services: {}\n")
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, release := make(chan struct{}), make(chan struct{})
	writer, err := openRootWithHooks(dir, nil, faultHooks{afterStageSync: func() error {
		close(staged)
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reclaimer, err := OpenRoot(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reclaimer.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write(context.Background(), WriteRequest{
			RelativePath: "compose.yaml", ExpectedSHA256: shaHex(original), Content: []byte("services:\n  app: {}\n"),
			Validate: func(context.Context, ValidationInput) error { return nil },
			Snapshot: func(context.Context, SnapshotInput) error { return nil },
			Commit:   func(context.Context) error { return nil },
		})
		writeDone <- err
	}()
	<-staged
	reclaimDone := make(chan error, 1)
	go func() {
		_, err := reclaimer.ReclaimAbandonedStagingForDiskPressure(context.Background(), 1)
		reclaimDone <- err
	}()
	select {
	case err := <-reclaimDone:
		t.Fatalf("reclaim did not wait for live write: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-reclaimDone; err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemSpaceRemainsBoundToOpenedRootAfterPathSwap(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "project")
	moved := filepath.Join(parent, "project-original")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	wantTotal, wantFree, err := root.FilesystemSpace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc", path); err != nil {
		t.Fatal(err)
	}
	gotTotal, gotFree, err := root.FilesystemSpace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotTotal != wantTotal || gotFree <= 0 || wantFree <= 0 {
		t.Fatalf("opened-root filesystem changed: before=%d/%d after=%d/%d", wantTotal, wantFree, gotTotal, gotFree)
	}
}
