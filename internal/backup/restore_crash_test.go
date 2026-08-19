package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// crashDuringRollback reproduces the on-disk state left by a hard kill in the
// middle of a rollback: stageArchive has already created the rollback staging
// files and some of them have been renamed onto their targets, so the ones that
// were not yet renamed survive the kill.
func TestRecoverResumesRollbackInterruptedMidway(t *testing.T) {
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
	if _, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "restore-crash",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	}); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("restore crash: %v", err)
	}

	// A rollback started and was killed after staging: one leftover rollback
	// staging file remains in the project directory.
	leftover := ".dockpilot-restore-" + restoreStageSuffix("restore-crash", "rollback") + "-000.tmp"
	mustWrite(t, filepath.Join(project.WorkingDir, leftover), "partial rollback stage", 0o600)

	restarted, err := New(manager.stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := restarted.Recover(context.Background(), ProjectResolverFunc(func(context.Context, string) (Project, error) {
		return project, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Interrupted || !results[0].RolledBack || results[0].RecoveryRequired || results[0].Err != nil {
		t.Fatalf("interrupted rollback was not resumed: %+v", results)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("resumed rollback did not reach the pre-restore state")
	}
	if err := restarted.CheckChangeAllowed(project.UID); err != nil {
		t.Fatalf("project stayed blocked after a resumable rollback: %v", err)
	}
	assertNoRestoreArtifacts(t, restarted, project)
	if _, err := os.Lstat(filepath.Join(project.WorkingDir, leftover)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback staging left orphaned: %v", err)
	}
}

func seedRestorePair(t *testing.T, manager *Manager, project Project) (string, string, Backup) {
	t.Helper()
	env := filepath.Join(project.WorkingDir, ".env")
	compose := filepath.Join(project.WorkingDir, "compose.yaml")
	mustWrite(t, env, "backup env", 0o600)
	mustWrite(t, compose, "backup compose", 0o600)
	source := createBackup(t, manager, project, TriggerManual, "source", time.Now(), ".env", "compose.yaml")
	mustWrite(t, env, "current env", 0o600)
	mustWrite(t, compose, "current compose", 0o600)
	return env, compose, source
}

func recoverOnce(t *testing.T, manager *Manager, project Project) []RecoveryResult {
	t.Helper()
	results, err := manager.Recover(context.Background(), ProjectResolverFunc(func(context.Context, string) (Project, error) {
		return project, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return results
}

// A kill inside staging leaves a PREPARING journal and whatever staging files
// had already been created. Nothing was replaced, so recovery must clean up.
func TestRecoverDiscardsPartialStagingFromPreparingCrash(t *testing.T) {
	manager, project := newTestManager(t)
	env, compose, source := seedRestorePair(t, manager, project)
	manifest, err := manager.loadManifest(project.UID, source.Manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	plan := stagePlan(manifest, restoreStageSuffix("staging-crash", "restore"))
	journal := restoreJournal{
		Version: journalVersion, OperationID: "staging-crash", ProjectUID: project.UID,
		BackupID: source.Manifest.BackupID, WorkingDir: project.WorkingDir, Phase: journalPreparing,
		PreRestoreSnapshotID: source.Manifest.BackupID,
	}
	for _, entry := range manifest.Files {
		journal.Files = append(journal.Files, journalFile{
			Target: entry.RelPath, StagedPath: plan[entry.RelPath], Status: filePending, OriginalExisted: true,
		})
	}
	if err := manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	// Only the first staging file made it to disk before the kill.
	mustWrite(t, filepath.Join(project.WorkingDir, journal.Files[0].StagedPath), "half written", 0o600)

	results := recoverOnce(t, manager, project)
	if len(results) != 1 || !results[0].Interrupted || results[0].RolledBack || results[0].RecoveryRequired || results[0].Err != nil {
		t.Fatalf("unexpected recovery: %+v", results)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("a PREPARING crash must not touch any target")
	}
	assertNoRestoreArtifacts(t, manager, project)
}

// COMMITTING was entered but no rename happened yet: the transaction is
// interrupted, not rolled back, and no staging survives.
func TestRecoverCommittingWithNoReplacementIsInterruptedNotRolledBack(t *testing.T) {
	manager, project := newTestManager(t)
	env, compose, source := seedRestorePair(t, manager, project)
	manifest, err := manager.loadManifest(project.UID, source.Manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	plan := stagePlan(manifest, restoreStageSuffix("commit-entry", "restore"))
	journal := restoreJournal{
		Version: journalVersion, OperationID: "commit-entry", ProjectUID: project.UID,
		BackupID: source.Manifest.BackupID, WorkingDir: project.WorkingDir, Phase: journalCommitting,
		PreRestoreSnapshotID: source.Manifest.BackupID,
	}
	for _, entry := range manifest.Files {
		journal.Files = append(journal.Files, journalFile{
			Target: entry.RelPath, StagedPath: plan[entry.RelPath], Status: filePending, OriginalExisted: true,
		})
		mustWrite(t, filepath.Join(project.WorkingDir, plan[entry.RelPath]), "staged", 0o600)
	}
	if err := manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	results := recoverOnce(t, manager, project)
	if len(results) != 1 || !results[0].Interrupted || results[0].RolledBack || results[0].RecoveryRequired {
		t.Fatalf("unexpected recovery: %+v", results)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("no target may change when nothing was replaced")
	}
	assertNoRestoreArtifacts(t, manager, project)
}

// The last file was replaced but the journal was never retired. The
// transaction never reported success, so recovery rolls the whole set back.
func TestRecoverRollsBackCrashAfterFinalReplacement(t *testing.T) {
	manager, project := newTestManager(t)
	env, compose, source := seedRestorePair(t, manager, project)
	manager.hooks.afterReplacement = func(index int) error {
		if index == 1 {
			return errSimulatedCrash
		}
		return nil
	}
	if _, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "finalizing",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	}); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("restore crash: %v", err)
	}
	if readString(t, env) != "backup env" || readString(t, compose) != "backup compose" {
		t.Fatalf("both replacements should have been committed before the crash")
	}
	restarted, err := New(manager.stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := recoverOnce(t, restarted, project)
	if len(results) != 1 || !results[0].Interrupted || !results[0].RolledBack || results[0].RecoveryRequired {
		t.Fatalf("unexpected recovery: %+v", results)
	}
	if readString(t, env) != "current env" || readString(t, compose) != "current compose" {
		t.Fatalf("rollback did not restore the pre-restore state")
	}
	assertNoRestoreArtifacts(t, restarted, project)
	if second := recoverOnce(t, restarted, project); len(second) != 0 {
		t.Fatalf("a closed transaction was recovered twice: %+v", second)
	}
}

// A rollback that keeps failing must stay blocked across restarts, and the
// block must survive as an on-disk journal rather than a process-local flag.
func TestRecoveryRequiredSurvivesRestartAndOnlyBlocksChanges(t *testing.T) {
	manager, project := newTestManager(t)
	_, _, source := seedRestorePair(t, manager, project)
	manager.hooks.afterReplacement = func(int) error { return errSimulatedCrash }
	if _, err := manager.Restore(context.Background(), RestoreRequest{
		Project: project, BackupID: source.Manifest.BackupID, OperationID: "blocked",
		CommitGate: CommitGateFunc(func(context.Context) error { return nil }),
	}); !errors.Is(err, errSimulatedCrash) {
		t.Fatal(err)
	}
	failure := errors.New("rollback device failure")
	for attempt := range 2 {
		restarted, err := New(manager.stateDir, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		restarted.hooks.rollbackFailure = func() error { return failure }
		results := recoverOnce(t, restarted, project)
		if len(results) != 1 || !results[0].RecoveryRequired || !errors.Is(results[0].Err, failure) {
			t.Fatalf("attempt %d: %+v", attempt, results)
		}
		if err := restarted.CheckChangeAllowed(project.UID); !errors.Is(err, ErrProjectRecoveryBlocked) {
			t.Fatalf("attempt %d: changes were not blocked: %v", attempt, err)
		}
		// Reads stay available while the project is blocked.
		if _, err := restarted.List(context.Background(), project.UID); err != nil {
			t.Fatalf("attempt %d: listing backups was blocked: %v", attempt, err)
		}
	}
	// Once the cause clears, the retained journal still closes the transaction.
	healed, err := New(manager.stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := recoverOnce(t, healed, project)
	if len(results) != 1 || !results[0].RolledBack || results[0].RecoveryRequired {
		t.Fatalf("recovery did not close once the cause cleared: %+v", results)
	}
	if err := healed.CheckChangeAllowed(project.UID); err != nil {
		t.Fatalf("project stayed blocked after a successful rollback: %v", err)
	}
	assertNoRestoreArtifacts(t, healed, project)
}
