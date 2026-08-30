package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type recordingBudget struct {
	admissions []Admission
	restores   []RestoreAdmission
	err        error
}

func (budget *recordingBudget) AdmitBackup(_ context.Context, admission Admission) error {
	budget.admissions = append(budget.admissions, admission)
	return budget.err
}

func (budget *recordingBudget) AdmitRestore(_ context.Context, admission RestoreAdmission) error {
	budget.restores = append(budget.restores, admission)
	return budget.err
}

type recordingIndex struct {
	metadata []Metadata
	err      error
}

func (index *recordingIndex) RecordBackup(_ context.Context, metadata Metadata) error {
	index.metadata = append(index.metadata, metadata)
	return index.err
}

func newTestManager(t *testing.T) (*Manager, Project) {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	projectDir := filepath.Join(base, "project")
	mustMkdir(t, state)
	mustMkdir(t, projectDir)
	manager, err := New(state, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manager, Project{UID: "project-1", Name: "example", WorkingDir: projectDir}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func createBackup(t *testing.T, manager *Manager, project Project, trigger Trigger, operation string, at time.Time, paths ...string) Backup {
	t.Helper()
	result, err := manager.Create(context.Background(), CreateRequest{
		Project: project, RelativePaths: paths, Trigger: trigger, OperationID: operation, CreatedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCreatePersistsSecureArchiveManifestAndMetadataBoundary(t *testing.T) {
	base := t.TempDir()
	state, working := filepath.Join(base, "state"), filepath.Join(base, "project")
	mustMkdir(t, state)
	mustMkdir(t, working)
	mustWrite(t, filepath.Join(working, "compose.yaml"), "services: {}\n", 0o640)
	mustWrite(t, filepath.Join(working, ".env"), "PASSWORD=secret\n", 0o600)
	budget, index := &recordingBudget{}, &recordingIndex{}
	manager, err := New(state, budget, index)
	if err != nil {
		t.Fatal(err)
	}
	project := Project{UID: "p-secure", Name: "secure", WorkingDir: working}
	at := time.Date(2026, 8, 15, 5, 6, 7, 8, time.UTC)
	backup := createBackup(t, manager, project, TriggerManual, "op-create", at, "compose.yaml", ".env")

	if backup.Manifest.CreatedAt != at || backup.Metadata.FileCount != 2 || len(index.metadata) != 1 || len(budget.admissions) != 1 {
		t.Fatalf("unexpected result: backup=%+v index=%+v budget=%+v", backup, index.metadata, budget.admissions)
	}
	if budget.admissions[0].EstimatedBytes <= int64(len("services: {}\n")+len("PASSWORD=secret\n")) {
		t.Fatalf("unexpected admission: %+v", budget.admissions[0])
	}
	if backup.Metadata.ManifestSHA256 == "" || backup.Metadata.SizeBytes <= 0 {
		t.Fatalf("metadata lacks manifest integrity/size: %+v", backup.Metadata)
	}
	backupDir := filepath.Join(state, "backups", project.UID, backup.Manifest.BackupID)
	assertMode(t, backupDir, 0o700)
	assertMode(t, filepath.Join(backupDir, "manifest.json"), 0o600)
	assertMode(t, filepath.Join(backupDir, "files.tar.gz"), 0o600)
	loaded, err := manager.LoadManifest(project.UID, backup.Manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkingDir != working || loaded.ProjectName != project.Name || len(loaded.Files) != 2 {
		t.Fatalf("unexpected manifest: %+v", loaded)
	}
	archive := readTar(t, filepath.Join(backupDir, "files.tar.gz"))
	if archive[".env"] != "PASSWORD=secret\n" || archive["compose.yaml"] != "services: {}\n" {
		t.Fatalf("unexpected archive: %#v", archive)
	}
}

func TestListReturnsMetadataOnlyNewestFirst(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "services: {}\n", 0o600)
	older := createBackup(t, manager, project, TriggerManual, "op-old", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "compose.yaml")
	newer := createBackup(t, manager, project, TriggerPreWrite, "op-new", time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), "compose.yaml")

	metadata, err := manager.List(context.Background(), project.UID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 2 || metadata[0].BackupID != newer.Metadata.BackupID || metadata[1].BackupID != older.Metadata.BackupID {
		t.Fatalf("metadata = %+v", metadata)
	}
	for _, item := range metadata {
		if item.ProjectUID != project.UID || item.ManifestSHA256 == "" || item.SizeBytes <= 0 || item.FileCount != 1 {
			t.Fatalf("unsafe or incomplete metadata = %+v", item)
		}
	}
	missing, err := manager.List(context.Background(), "no-backups")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing project metadata = %+v, %v", missing, err)
	}
}

func TestCreateRejectsUnsafeSourcesAndBudgetDenialLeavesNoBackup(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "ok", 0o600)
	if err := os.Symlink("compose.yaml", filepath.Join(project.WorkingDir, ".env")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../compose.yaml", "/tmp/compose.yaml", "sub/compose.yaml", "notes.txt", "compose.override.txt", "compose.custom.yml", "docker-compose.custom.yaml"} {
		_, err := manager.Create(context.Background(), CreateRequest{Project: project, RelativePaths: []string{path}, Trigger: TriggerManual, OperationID: "op"})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q: got %v", path, err)
		}
	}
	_, err := manager.Create(context.Background(), CreateRequest{Project: project, RelativePaths: []string{".env"}, Trigger: TriggerManual, OperationID: "op"})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink: got %v", err)
	}

	state := filepath.Join(t.TempDir(), "state")
	mustMkdir(t, state)
	denied := errors.New("disk budget denied")
	manager, err = New(state, &recordingBudget{err: denied}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(context.Background(), CreateRequest{Project: project, RelativePaths: []string{"compose.yaml"}, Trigger: TriggerManual, OperationID: "denied"})
	if !errors.Is(err, denied) {
		t.Fatalf("got %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "backups"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("budget denial left backup data: entries=%v err=%v", entries, err)
	}
}

func TestIdentifiersCannotEscapeBackupOrJournalRoots(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "ok", 0o600)
	for _, id := range []string{".", ".."} {
		badProject := project
		badProject.UID = id
		if _, err := manager.Create(context.Background(), CreateRequest{
			Project: badProject, RelativePaths: []string{"compose.yaml"}, Trigger: TriggerManual, OperationID: "op",
		}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("project ID %q error = %v", id, err)
		}
		if _, err := manager.Create(context.Background(), CreateRequest{
			Project: project, RelativePaths: []string{"compose.yaml"}, Trigger: TriggerManual, OperationID: id,
		}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("operation ID %q error = %v", id, err)
		}
	}
}

func TestCanceledCreateAndRestoreLeaveNoStaging(t *testing.T) {
	manager, project := newTestManager(t)
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, compose, "backup", 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := manager.Create(ctx, CreateRequest{
		Project: project, RelativePaths: []string{"compose.yaml"}, Trigger: TriggerManual, OperationID: "create-canceled",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create got %v", err)
	}
	projectBackupDir := filepath.Join(manager.backupDir, project.UID)
	if entries, readErr := os.ReadDir(projectBackupDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("canceled create artifacts=%v err=%v", entries, readErr)
	}

	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
	mustWrite(t, compose, "current", 0o600)
	gateCalled := false
	_, err = manager.Restore(ctx, RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-canceled-context",
		CommitGate: CommitGateFunc(func(context.Context) error { gateCalled = true; return nil }),
	})
	if !errors.Is(err, context.Canceled) || gateCalled || readString(t, compose) != "current" {
		t.Fatalf("restore err=%v gate=%v contents=%q", err, gateCalled, readString(t, compose))
	}
	assertNoRestoreArtifacts(t, manager, project)
}

func TestAutomaticRetentionKeepsTwentyAndNeverDeletesManual(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "services: {}\n", 0o600)
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	manual := createBackup(t, manager, project, TriggerManual, "manual", base, "compose.yaml")
	for index := 0; index < AutomaticSnapshotRetention+3; index++ {
		createBackup(t, manager, project, TriggerPreWrite, "auto-"+time.Duration(index).String(), base.Add(time.Duration(index+1)*time.Second), "compose.yaml")
	}
	if _, err := manager.PruneAutomatic(project.UID, AutomaticSnapshotRetention); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(manager.backupDir, project.UID)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != AutomaticSnapshotRetention+1 {
		t.Fatalf("got %d backup directories, want %d", len(entries), AutomaticSnapshotRetention+1)
	}
	if _, err := os.Stat(filepath.Join(projectDir, manual.Manifest.BackupID)); err != nil {
		t.Fatalf("manual backup was deleted: %v", err)
	}
	for _, entry := range entries {
		manifest, err := manager.LoadManifest(project.UID, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Trigger == TriggerManual && manifest.BackupID != manual.Manifest.BackupID {
			t.Fatalf("unexpected manual backup: %+v", manifest)
		}
	}
}

func TestRestoreSuccessCreatesPreRestoreSnapshotAndUsesCommitGate(t *testing.T) {
	manager, project := newTestManager(t)
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	env := filepath.Join(project.WorkingDir, ".env")
	mustWrite(t, compose, "old compose", 0o640)
	mustWrite(t, env, "OLD=1", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml", ".env")
	budget := &recordingBudget{}
	manager.budget = budget
	mustWrite(t, compose, "current compose", 0o600)
	mustWrite(t, env, "CURRENT=1", 0o640)
	gateCalls := 0
	result, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-success",
		CommitGate: CommitGateFunc(func(context.Context) error { gateCalls++; return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gateCalls != 1 || result.RestoredFiles != 2 || result.PreRestoreSnapshotID == "" || result.RolledBack {
		t.Fatalf("unexpected result=%+v gate calls=%d", result, gateCalls)
	}
	if len(budget.restores) != 1 || budget.restores[0].ProjectUID != project.UID ||
		budget.restores[0].BackupID != source.Manifest.BackupID || budget.restores[0].FilesystemTotalBytes <= 0 ||
		budget.restores[0].FilesystemFreeBytes <= 0 || budget.restores[0].EstimatedBytes != int64(len("old compose")+len("OLD=1")) {
		t.Fatalf("restore admission = %+v", budget.restores)
	}
	if got := readString(t, compose); got != "old compose" {
		t.Fatalf("compose=%q", got)
	}
	if got := readString(t, env); got != "OLD=1" {
		t.Fatalf("env=%q", got)
	}
	assertMode(t, compose, 0o640)
	assertMode(t, env, 0o600)
	pre, err := manager.LoadManifest(project.UID, result.PreRestoreSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Trigger != TriggerPreRestore {
		t.Fatalf("pre-restore trigger=%q", pre.Trigger)
	}
	preArchive := readTar(t, filepath.Join(manager.backupDir, project.UID, pre.BackupID, "files.tar.gz"))
	if preArchive["compose.yaml"] != "current compose" || preArchive[".env"] != "CURRENT=1" {
		t.Fatalf("pre-restore archive=%#v", preArchive)
	}
	assertNoRestoreArtifacts(t, manager, project)
}

func TestRestoreCancellationBeforeCommitDoesNotReplaceFiles(t *testing.T) {
	manager, project := newTestManager(t)
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, compose, "backup", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
	mustWrite(t, compose, "current", 0o600)
	canceled := errors.New("cancel was accepted")
	result, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-cancel",
		CommitGate: CommitGateFunc(func(context.Context) error { return canceled }),
	})
	if !errors.Is(err, ErrCommitCanceled) || !errors.Is(err, canceled) {
		t.Fatalf("got %v", err)
	}
	if result.PreRestoreSnapshotID == "" || readString(t, compose) != "current" {
		t.Fatalf("unexpected result=%+v contents=%q", result, readString(t, compose))
	}
	assertNoRestoreArtifacts(t, manager, project)
}

func TestRestoreFailureRollsBackPartialReplacement(t *testing.T) {
	manager, project := newTestManager(t)
	env := filepath.Join(project.WorkingDir, ".env")
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, env, "backup env", 0o600)
	mustWrite(t, compose, "backup compose", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), ".env", "compose.yaml")
	mustWrite(t, env, "current env", 0o600)
	mustWrite(t, compose, "current compose", 0o600)
	injected := errors.New("replacement failure")
	manager.hooks.afterReplacement = func(index int) error {
		if index == 0 {
			return injected
		}
		return nil
	}
	result, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-rollback",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	})
	if !errors.Is(err, injected) || !result.RolledBack {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("rollback did not restore current state")
	}
	assertNoRestoreArtifacts(t, manager, project)
}

func TestRollbackRemovesTargetThatDidNotExistBeforeRestore(t *testing.T) {
	manager, project := newTestManager(t)
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, compose, "backup", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
	if err := os.Remove(compose); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("fail after create")
	manager.hooks.afterReplacement = func(int) error { return injected }
	result, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-missing",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	})
	if !errors.Is(err, injected) || !result.RolledBack {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Lstat(compose); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new target remains after rollback: %v", err)
	}
	assertNoRestoreArtifacts(t, manager, project)
}

func TestRecoverRollsBackCrashAfterPartialCommit(t *testing.T) {
	manager, project := newTestManager(t)
	env := filepath.Join(project.WorkingDir, ".env")
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, env, "backup env", 0o600)
	mustWrite(t, compose, "backup compose", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), ".env", "compose.yaml")
	mustWrite(t, env, "current env", 0o600)
	mustWrite(t, compose, "current compose", 0o600)
	manager.hooks.afterReplacement = func(index int) error {
		if index == 0 {
			return errSimulatedCrash
		}
		return nil
	}
	_, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-crash",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	})
	if !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("got %v", err)
	}
	if readString(t, env) != "backup env" {
		t.Fatalf("first replacement was not committed before crash")
	}

	restarted, err := New(manager.stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := restarted.Recover(context.Background(), ProjectResolverFunc(func(_ context.Context, uid string) (Project, error) {
		if uid != project.UID {
			t.Fatalf("unexpected uid %q", uid)
		}
		return project, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Interrupted || !results[0].RolledBack || results[0].RecoveryRequired || results[0].Err != nil {
		t.Fatalf("unexpected recovery: %+v", results)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("recovery did not roll back to pre-restore state")
	}
	assertNoRestoreArtifacts(t, restarted, project)
}

func TestRecoveryFailureBlocksProjectChanges(t *testing.T) {
	manager, project := newTestManager(t)
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, compose, "backup", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
	mustWrite(t, compose, "current", 0o600)
	manager.hooks.afterReplacement = func(int) error { return errSimulatedCrash }
	_, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-block",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	})
	if !errors.Is(err, errSimulatedCrash) {
		t.Fatal(err)
	}

	restarted, err := New(manager.stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rollbackFailure := errors.New("disk unavailable")
	restarted.hooks.rollbackFailure = func() error { return rollbackFailure }
	results, err := restarted.Recover(context.Background(), ProjectResolverFunc(func(context.Context, string) (Project, error) { return project, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].RecoveryRequired || !errors.Is(results[0].Err, rollbackFailure) {
		t.Fatalf("unexpected results: %+v", results)
	}
	if err := restarted.CheckChangeAllowed(project.UID); !errors.Is(err, ErrProjectRecoveryBlocked) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("project was not blocked: %v", err)
	}
	_, err = restarted.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-again",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	})
	if !errors.Is(err, ErrProjectRecoveryBlocked) {
		t.Fatalf("blocked restore got %v", err)
	}
}

func TestRecoverPreparingJournalCleansStagingAsInterrupted(t *testing.T) {
	manager, project := newTestManager(t)
	mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "current", 0o600)
	backup := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
	stage := ".docklattice-restore-prepare-000.tmp"
	mustWrite(t, filepath.Join(project.WorkingDir, stage), "staged", 0o600)
	journal := restoreJournal{
		Version: journalVersion, OperationID: "prepare", ProjectUID: project.UID,
		BackupID: backup.Manifest.BackupID, WorkingDir: project.WorkingDir, Phase: journalPreparing,
		PreRestoreSnapshotID: backup.Manifest.BackupID,
		Files:                []journalFile{{Target: "compose.yaml", StagedPath: stage, Status: filePending, OriginalExisted: true}},
	}
	if err := manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	results, err := manager.Recover(context.Background(), ProjectResolverFunc(func(context.Context, string) (Project, error) { return project, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Interrupted || results[0].RolledBack || results[0].Err != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	if _, err := os.Stat(filepath.Join(project.WorkingDir, stage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging remains: %v", err)
	}
}

func TestRestoreRejectsTraversalAndSymlinkArchivesAndTargetSymlink(t *testing.T) {
	t.Run("traversal tar", func(t *testing.T) {
		manager, project := newTestManager(t)
		compose := filepath.Join(project.WorkingDir, "compose.yaml")
		mustWrite(t, compose, "safe", 0o600)
		backup := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
		archivePath := filepath.Join(manager.backupDir, project.UID, backup.Manifest.BackupID, "files.tar.gz")
		writeTar(t, archivePath, []tarRecord{{name: "../escape", body: "owned", mode: 0o600}})
		_, err := manager.Restore(context.Background(), RestoreRequest{
			Project: project, BackupID: backup.Manifest.BackupID, OperationID: "bad-tar",
			CommitGate: CommitGateFunc(func(context.Context) error { t.Fatal("gate called"); return nil }),
		})
		if !errors.Is(err, ErrInvalidArchive) || readString(t, compose) != "safe" {
			t.Fatalf("err=%v contents=%q", err, readString(t, compose))
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(project.WorkingDir), "escape")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("archive escaped project: %v", err)
		}
	})

	t.Run("tar symlink entry", func(t *testing.T) {
		manager, project := newTestManager(t)
		compose := filepath.Join(project.WorkingDir, "compose.yaml")
		mustWrite(t, compose, "safe", 0o600)
		backup := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
		archivePath := filepath.Join(manager.backupDir, project.UID, backup.Manifest.BackupID, "files.tar.gz")
		writeTar(t, archivePath, []tarRecord{{name: "compose.yaml", mode: 0o600, typeflag: tar.TypeSymlink, linkname: "../outside"}})
		_, err := manager.Restore(context.Background(), RestoreRequest{
			Project: project, BackupID: backup.Manifest.BackupID, OperationID: "tar-link",
			CommitGate: CommitGateFunc(func(context.Context) error { t.Fatal("gate called"); return nil }),
		})
		if !errors.Is(err, ErrInvalidArchive) || readString(t, compose) != "safe" {
			t.Fatalf("err=%v contents=%q", err, readString(t, compose))
		}
	})

	t.Run("archive symlink", func(t *testing.T) {
		manager, project := newTestManager(t)
		mustWrite(t, filepath.Join(project.WorkingDir, "compose.yaml"), "safe", 0o600)
		backup := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
		archivePath := filepath.Join(manager.backupDir, project.UID, backup.Manifest.BackupID, "files.tar.gz")
		if err := os.Remove(archivePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("manifest.json", archivePath); err != nil {
			t.Fatal(err)
		}
		_, err := manager.Restore(context.Background(), RestoreRequest{
			Project: project, BackupID: backup.Manifest.BackupID, OperationID: "bad-link",
			CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
		})
		if !errors.Is(err, ErrSymlink) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("target symlink", func(t *testing.T) {
		manager, project := newTestManager(t)
		compose := filepath.Join(project.WorkingDir, "compose.yaml")
		outside := filepath.Join(filepath.Dir(project.WorkingDir), "outside")
		mustWrite(t, compose, "backup", 0o600)
		mustWrite(t, outside, "outside", 0o600)
		backup := createBackup(t, manager, project, TriggerManual, "source", time.Now(), "compose.yaml")
		if err := os.Remove(compose); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, compose); err != nil {
			t.Fatal(err)
		}
		_, err := manager.Restore(context.Background(), RestoreRequest{
			Project: project, BackupID: backup.Manifest.BackupID, OperationID: "bad-target",
			CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
		})
		if !errors.Is(err, ErrSymlink) || readString(t, outside) != "outside" {
			t.Fatalf("err=%v outside=%q", err, readString(t, outside))
		}
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o want=%#o", path, got, want)
	}
}

func assertNoRestoreArtifacts(t *testing.T, manager *Manager, project Project) {
	t.Helper()
	journalEntries, err := os.ReadDir(manager.journalDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(journalEntries) != 0 {
		t.Fatalf("journal artifacts remain: %v", journalEntries)
	}
	projectEntries, err := os.ReadDir(project.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range projectEntries {
		if strings.HasPrefix(entry.Name(), ".docklattice-restore-") {
			t.Fatalf("staging artifact remains: %s", entry.Name())
		}
	}
}

type tarRecord struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
}

func writeTar(t *testing.T, path string, records []tarRecord) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, record := range records {
		typeflag := record.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: record.name, Size: int64(len(record.body)), Mode: record.mode, Typeflag: typeflag, Linkname: record.linkname}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, record.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func readTar(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tw := tar.NewReader(gz)
	result := make(map[string]string)
	for {
		header, err := tw.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(tw)
		if err != nil {
			t.Fatal(err)
		}
		result[header.Name] = string(payload)
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return result
}
