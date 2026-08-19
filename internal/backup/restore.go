package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const journalVersion = 1

const (
	journalPreparing  = "PREPARING"
	journalCommitting = "COMMITTING"
	filePending       = "pending"
	fileReplaced      = "replaced"
)

var errSimulatedCrash = errors.New("simulated restore crash")

type restoreJournal struct {
	Version              int           `json:"version"`
	OperationID          string        `json:"operation_id"`
	ProjectUID           string        `json:"project_uid"`
	BackupID             string        `json:"backup_id"`
	WorkingDir           string        `json:"working_dir"`
	Phase                string        `json:"phase"`
	PreRestoreSnapshotID string        `json:"pre_restore_snapshot_id"`
	Files                []journalFile `json:"files"`
}

type journalFile struct {
	Target          string `json:"target"`
	StagedPath      string `json:"staged_path"`
	Status          string `json:"status"`
	OriginalExisted bool   `json:"original_existed"`
}

func (manager *Manager) Restore(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	if err := validateProject(request.Project); err != nil {
		return RestoreResult{}, err
	}
	if !validBackupID(request.BackupID) || !validSafeID(request.OperationID) || request.CommitGate == nil {
		return RestoreResult{}, ErrInvalidPath
	}
	if err := manager.CheckChangeAllowed(request.Project.UID); err != nil {
		return RestoreResult{}, err
	}
	manifest, err := manager.loadManifest(request.Project.UID, request.BackupID)
	if err != nil {
		return RestoreResult{}, err
	}
	if manifest.WorkingDir != request.Project.WorkingDir {
		return RestoreResult{}, fmt.Errorf("%w: backup working directory does not match project", ErrInvalidArchive)
	}
	root, err := os.OpenRoot(request.Project.WorkingDir)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("backup: open project root: %w", err)
	}
	defer root.Close()
	if manager.budget != nil {
		var estimated int64
		for _, file := range manifest.Files {
			estimated += file.Size
		}
		total, free, err := openedRootFilesystemSpace(ctx, root)
		if err != nil {
			return RestoreResult{}, err
		}
		if err := manager.budget.AdmitRestore(ctx, RestoreAdmission{
			ProjectUID: request.Project.UID, BackupID: request.BackupID,
			FilesystemTotalBytes: total, FilesystemFreeBytes: free, EstimatedBytes: estimated,
		}); err != nil {
			return RestoreResult{}, err
		}
	}
	existing, err := existingTargets(root, manifest.Files)
	if err != nil {
		return RestoreResult{}, err
	}
	preRestore, err := manager.create(ctx, CreateRequest{
		Project: request.Project, RelativePaths: existing, Trigger: TriggerPreRestore,
		OperationID: request.OperationID, CreatedAt: request.Now,
	}, true)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("backup: create pre-restore snapshot: %w", err)
	}
	original := make(map[string]bool, len(preRestore.Manifest.Files))
	for _, entry := range preRestore.Manifest.Files {
		original[entry.RelPath] = true
	}
	restoreSuffix := restoreStageSuffix(request.OperationID, "restore")
	staged := stagePlan(manifest, restoreSuffix)
	journal := restoreJournal{
		Version: journalVersion, OperationID: request.OperationID, ProjectUID: request.Project.UID,
		BackupID: request.BackupID, WorkingDir: request.Project.WorkingDir, Phase: journalPreparing,
		PreRestoreSnapshotID: preRestore.Manifest.BackupID,
		Files:                make([]journalFile, 0, len(manifest.Files)),
	}
	for _, entry := range manifest.Files {
		journal.Files = append(journal.Files, journalFile{
			Target: entry.RelPath, StagedPath: staged[entry.RelPath], Status: filePending,
			OriginalExisted: original[entry.RelPath],
		})
	}
	// The journal is written before a single staged byte exists so that no
	// staging file can ever outlive the transaction that owns it: a crash from
	// here on always leaves a journal that names every staged path to remove.
	if err := manager.writeJournal(journal); err != nil {
		return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID}, err
	}
	// Until the transaction enters COMMITTING nothing has been replaced, so any
	// early return discards the whole transaction - journal and staging alike.
	discardOnReturn := true
	defer func() {
		if discardOnReturn {
			_ = manager.discardJournal(root, journal)
		}
	}()
	if err := manager.stageArchive(ctx, root, manifest, restoreSuffix); err != nil {
		return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID}, err
	}
	if err := request.CommitGate.EnterRestoreCommit(ctx); err != nil {
		cleanupErr := manager.discardJournal(root, journal)
		return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID}, errors.Join(ErrCommitCanceled, err, cleanupErr)
	}
	journal.Phase = journalCommitting
	if err := manager.writeJournal(journal); err != nil {
		cleanupErr := manager.discardJournal(root, journal)
		return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID}, errors.Join(err, cleanupErr)
	}
	// From here the journal outlives this call unless a replacement path
	// deliberately retires it: recovery needs it to close the transaction.
	discardOnReturn = false

	for index := range journal.Files {
		// Persisting "replaced" first is deliberately conservative. A crash after
		// this write but before rename causes an unnecessary but safe rollback.
		journal.Files[index].Status = fileReplaced
		if err := manager.writeJournal(journal); err != nil {
			return manager.restoreFailure(root, journal, err)
		}
		if err := root.Rename(journal.Files[index].StagedPath, journal.Files[index].Target); err != nil {
			return manager.restoreFailure(root, journal, fmt.Errorf("backup: replace %q: %w", journal.Files[index].Target, err))
		}
		if err := syncRoot(root); err != nil {
			return manager.restoreFailure(root, journal, err)
		}
		if manager.hooks.afterReplacement != nil {
			if err := manager.hooks.afterReplacement(index); err != nil {
				if errors.Is(err, errSimulatedCrash) {
					return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID, RestoredFiles: index + 1}, err
				}
				return manager.restoreFailure(root, journal, err)
			}
		}
		if request.Progress != nil {
			if err := request.Progress(index+1, len(journal.Files)); err != nil {
				return manager.restoreFailure(root, journal, err)
			}
		}
	}
	if err := manager.removeJournal(journal.OperationID); err != nil {
		manager.block(journal.ProjectUID, err)
		return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID, RestoredFiles: len(journal.Files)}, errors.Join(ErrRecoveryRequired, err)
	}
	return RestoreResult{PreRestoreSnapshotID: preRestore.Manifest.BackupID, RestoredFiles: len(journal.Files)}, nil
}

func (manager *Manager) restoreFailure(root *os.Root, journal restoreJournal, cause error) (RestoreResult, error) {
	result := RestoreResult{PreRestoreSnapshotID: journal.PreRestoreSnapshotID}
	if err := manager.rollback(root, journal); err != nil {
		manager.block(journal.ProjectUID, err)
		return result, errors.Join(cause, ErrRecoveryRequired, err)
	}
	result.RolledBack = true
	if err := manager.discardJournal(root, journal); err != nil {
		manager.block(journal.ProjectUID, err)
		return result, errors.Join(cause, ErrRecoveryRequired, err)
	}
	return result, cause
}

func existingTargets(root *os.Root, entries []FileEntry) ([]string, error) {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := root.Lstat(entry.RelPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %q", ErrSymlink, entry.RelPath)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %q is not a regular file", ErrInvalidPath, entry.RelPath)
		}
		paths = append(paths, entry.RelPath)
	}
	return paths, nil
}

// stagePlan names one staging file per manifest entry. The name is derived
// only from the transaction and the manifest order, so a retry of the same
// transaction reuses exactly the same names instead of leaking new ones.
func stagePlan(manifest Manifest, suffix string) map[string]string {
	plan := make(map[string]string, len(manifest.Files))
	for index, entry := range manifest.Files {
		plan[entry.RelPath] = fmt.Sprintf(".dockpilot-restore-%s-%03d.tmp", suffix, index)
	}
	return plan
}

func stagePrefix(suffix string) string { return ".dockpilot-restore-" + suffix + "-" }

// purgeStaging removes the staging files of one transaction and purpose. Only
// names carrying that transaction's own prefix are touched, and a crashed
// attempt is the only thing that can have left them behind.
func purgeStaging(root *os.Root, suffix string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := directory.Readdirnames(-1)
	directory.Close()
	if err != nil {
		return err
	}
	prefix := stagePrefix(suffix)
	removed := false
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: remove stale restore staging %q: %w", name, err)
		}
		removed = true
	}
	if !removed {
		return nil
	}
	return syncRoot(root)
}

func (manager *Manager) stageArchive(ctx context.Context, root *os.Root, manifest Manifest, suffix string) error {
	if !validSafeID(suffix) {
		return ErrInvalidPath
	}
	plan := stagePlan(manifest, suffix)
	if err := purgeStaging(root, suffix); err != nil {
		return err
	}
	backupPath := filepath.Join(manager.backupDir, manifest.ProjectUID, manifest.BackupID)
	if err := validateStoredBackupDirectory(backupPath); err != nil {
		return err
	}
	archivePath := filepath.Join(backupPath, "files.tar.gz")
	archive, err := openSecureRegular(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("%w: gzip: %v", ErrInvalidArchive, err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	expected := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		expected[entry.RelPath] = entry
	}
	staged := make(map[string]struct{}, len(expected))
	cleanup := true
	defer func() {
		if cleanup {
			removeStaged(root, plan)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: tar: %v", ErrInvalidArchive, err)
		}
		entry, ok := expected[header.Name]
		if !ok || !validManagedPath(header.Name) || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			return fmt.Errorf("%w: unexpected archive entry %q", ErrInvalidArchive, header.Name)
		}
		if _, duplicate := staged[header.Name]; duplicate {
			return fmt.Errorf("%w: duplicate archive entry %q", ErrInvalidArchive, header.Name)
		}
		if header.Size != entry.Size || uint32(header.Mode)&0o777 != entry.Mode {
			return fmt.Errorf("%w: metadata mismatch for %q", ErrInvalidArchive, header.Name)
		}
		file, err := root.OpenFile(plan[header.Name], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("backup: create restore staging file: %w", err)
		}
		staged[header.Name] = struct{}{}
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(file, hash), contextReader{ctx: ctx, reader: tarReader}, entry.Size)
		if copyErr == nil && written != entry.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr == nil {
			copyErr = file.Chmod(os.FileMode(entry.Mode))
		}
		if copyErr == nil {
			copyErr = file.Sync()
		}
		if closeErr := file.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return fmt.Errorf("%w: extract %q: %v", ErrInvalidArchive, header.Name, copyErr)
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), entry.SHA256) {
			return fmt.Errorf("%w: digest mismatch for %q", ErrInvalidArchive, header.Name)
		}
	}
	if len(staged) != len(expected) {
		return fmt.Errorf("%w: archive entries do not match manifest", ErrInvalidArchive)
	}
	if err := syncRoot(root); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func validateStoredBackupDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrSymlink
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return ErrInvalidArchive
	}
	return nil
}

func openSecureRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSymlink
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, ErrInvalidArchive
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) || !after.Mode().IsRegular() {
		file.Close()
		return nil, ErrConcurrentModification
	}
	return file, nil
}

func removeStaged(root *os.Root, staged map[string]string) {
	for _, path := range staged {
		_ = root.Remove(path)
	}
	_ = syncRoot(root)
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (manager *Manager) writeJournal(journal restoreJournal) error {
	if err := validateJournal(journal); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary := filepath.Join(manager.journalDir, "."+journal.OperationID+".tmp")
	final := filepath.Join(manager.journalDir, journal.OperationID+".json")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if removeErr := os.Remove(temporary); removeErr != nil {
			return removeErr
		}
		file, err = os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, final); err != nil {
		os.Remove(temporary)
		return err
	}
	return syncDirectory(manager.journalDir)
}

func (manager *Manager) loadJournal(path string) (restoreJournal, error) {
	file, err := openSecureRegular(path)
	if err != nil {
		return restoreJournal{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var journal restoreJournal
	if err := decoder.Decode(&journal); err != nil {
		return restoreJournal{}, fmt.Errorf("%w: journal: %v", ErrRecoveryRequired, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return restoreJournal{}, fmt.Errorf("%w: trailing journal data", ErrRecoveryRequired)
	}
	if err := validateJournal(journal); err != nil {
		return restoreJournal{}, err
	}
	return journal, nil
}

func validateJournal(journal restoreJournal) error {
	if journal.Version != journalVersion || !validSafeID(journal.OperationID) || !validSafeID(journal.ProjectUID) ||
		!validBackupID(journal.BackupID) || !validBackupID(journal.PreRestoreSnapshotID) || !filepath.IsAbs(journal.WorkingDir) ||
		(journal.Phase != journalPreparing && journal.Phase != journalCommitting) {
		return ErrRecoveryRequired
	}
	seen := make(map[string]struct{}, len(journal.Files))
	for _, file := range journal.Files {
		if !validManagedPath(file.Target) || filepath.Base(file.StagedPath) != file.StagedPath ||
			!strings.HasPrefix(file.StagedPath, ".dockpilot-restore-") || (file.Status != filePending && file.Status != fileReplaced) {
			return ErrRecoveryRequired
		}
		if _, exists := seen[file.Target]; exists {
			return ErrRecoveryRequired
		}
		seen[file.Target] = struct{}{}
	}
	return nil
}

func (manager *Manager) removeJournal(operationID string) error {
	if !validSafeID(operationID) {
		return ErrInvalidPath
	}
	err := os.Remove(filepath.Join(manager.journalDir, operationID+".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(manager.journalDir)
}

func (manager *Manager) discardJournal(root *os.Root, journal restoreJournal) error {
	staged := make(map[string]string, len(journal.Files))
	for _, file := range journal.Files {
		staged[file.Target] = file.StagedPath
	}
	removeStaged(root, staged)
	return manager.removeJournal(journal.OperationID)
}

func (manager *Manager) rollback(root *os.Root, journal restoreJournal) error {
	if manager.hooks.rollbackFailure != nil {
		if err := manager.hooks.rollbackFailure(); err != nil {
			return err
		}
	}
	manifest, err := manager.loadManifest(journal.ProjectUID, journal.PreRestoreSnapshotID)
	if err != nil {
		return err
	}
	if manifest.WorkingDir != journal.WorkingDir {
		return ErrInvalidArchive
	}
	rollbackSuffix := restoreStageSuffix(journal.OperationID, "rollback")
	rollbackStaged := stagePlan(manifest, rollbackSuffix)
	if err := manager.stageArchive(context.Background(), root, manifest, rollbackSuffix); err != nil {
		return err
	}
	defer removeStaged(root, rollbackStaged)
	for _, file := range journal.Files {
		if file.Status != fileReplaced {
			continue
		}
		if file.OriginalExisted {
			stage, ok := rollbackStaged[file.Target]
			if !ok {
				return fmt.Errorf("%w: pre-restore snapshot missing %q", ErrInvalidArchive, file.Target)
			}
			if err := root.Rename(stage, file.Target); err != nil {
				return err
			}
		} else if err := root.Remove(file.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncRoot(root); err != nil {
			return err
		}
	}
	return nil
}

func (manager *Manager) Recover(ctx context.Context, resolver ProjectResolver) ([]RecoveryResult, error) {
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	if resolver == nil {
		return nil, ErrInvalidPath
	}
	entries, err := os.ReadDir(manager.journalDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	results := make([]RecoveryResult, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		journal, loadErr := manager.loadJournal(filepath.Join(manager.journalDir, entry.Name()))
		if loadErr != nil {
			return results, loadErr
		}
		result := RecoveryResult{OperationID: journal.OperationID, ProjectUID: journal.ProjectUID}
		project, resolveErr := resolver.ResolveBackupProject(ctx, journal.ProjectUID)
		if resolveErr != nil {
			result.RecoveryRequired, result.Err = true, resolveErr
			manager.block(journal.ProjectUID, resolveErr)
			results = append(results, result)
			continue
		}
		if validateProject(project) != nil || project.WorkingDir != journal.WorkingDir {
			resolveErr = fmt.Errorf("%w: project resolution mismatch", ErrRecoveryRequired)
			result.RecoveryRequired, result.Err = true, resolveErr
			manager.block(journal.ProjectUID, resolveErr)
			results = append(results, result)
			continue
		}
		root, openErr := os.OpenRoot(project.WorkingDir)
		if openErr != nil {
			result.RecoveryRequired, result.Err = true, openErr
			manager.block(journal.ProjectUID, openErr)
			results = append(results, result)
			continue
		}
		replaced := false
		for _, file := range journal.Files {
			replaced = replaced || file.Status == fileReplaced
		}
		if journal.Phase == journalPreparing || !replaced {
			result.Interrupted = true
			// Nothing was replaced, but a journal that cannot be retired would
			// be replayed on every boot: that is not a clean close either.
			if result.Err = manager.discardJournal(root, journal); result.Err != nil {
				result.RecoveryRequired = true
				manager.block(journal.ProjectUID, result.Err)
			}
		} else if rollbackErr := manager.rollback(root, journal); rollbackErr != nil {
			result.Interrupted, result.RecoveryRequired, result.Err = true, true, rollbackErr
			manager.block(journal.ProjectUID, rollbackErr)
		} else {
			result.Interrupted, result.RolledBack = true, true
			result.Err = manager.discardJournal(root, journal)
			if result.Err != nil {
				result.RecoveryRequired = true
				manager.block(journal.ProjectUID, result.Err)
			}
		}
		root.Close()
		results = append(results, result)
	}
	return results, nil
}

func (manager *Manager) block(projectUID string, cause error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.blocked[projectUID] = cause
}

func (manager *Manager) CheckChangeAllowed(projectUID string) error {
	if !validSafeID(projectUID) {
		return ErrInvalidPath
	}
	manager.mu.RLock()
	cause, blocked := manager.blocked[projectUID]
	manager.mu.RUnlock()
	if blocked {
		return errors.Join(ErrProjectRecoveryBlocked, cause)
	}
	return nil
}

func (manager *Manager) RecoveryBlocked(projectUID string) bool {
	manager.mu.RLock()
	_, blocked := manager.blocked[projectUID]
	manager.mu.RUnlock()
	return blocked
}

func restoreStageSuffix(operationID, purpose string) string {
	digest := sha256.Sum256([]byte(operationID + ":" + purpose))
	return purpose + "-" + hex.EncodeToString(digest[:8])
}
