package backup

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type automaticCandidate struct {
	projectUID string
	backupID   string
	createdAt  time.Time
	bytes      int64
}

// ReclaimAbandonedTempForDiskPressure removes only temporary objects whose
// owning Create/Restore/journal write is no longer active. storageMu is shared
// by all those writers, so observing a matching temp while holding it proves
// that the temp was abandoned.
func (manager *Manager) ReclaimAbandonedTempForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	var candidates []string
	journalEntries, err := os.ReadDir(manager.journalDir)
	if err != nil {
		return 0, err
	}
	for _, entry := range journalEntries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".tmp") {
			candidates = append(candidates, filepath.Join(manager.journalDir, entry.Name()))
		}
	}
	projects, err := os.ReadDir(manager.backupDir)
	if err != nil {
		return 0, err
	}
	for _, project := range projects {
		if !project.IsDir() || !validSafeID(project.Name()) {
			continue
		}
		projectDir := filepath.Join(manager.backupDir, project.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") && validBackupID(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp")) {
				candidates = append(candidates, filepath.Join(projectDir, name))
			}
		}
	}
	sort.Strings(candidates)
	var freed int64
	for _, path := range candidates {
		if freed >= bytesNeeded {
			break
		}
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		bytes, err := secureLogicalBytes(path)
		if err != nil {
			return freed, err
		}
		if err := removeSecurePath(path); err != nil {
			return freed, err
		}
		freed += bytes
	}
	return freed, nil
}

// ReclaimExcessAutomaticForDiskPressure deletes automatic snapshots beyond
// keep per project. Manual backups never enter the candidate set.
func (manager *Manager) ReclaimExcessAutomaticForDiskPressure(ctx context.Context, bytesNeeded int64, keep int) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	if keep < 1 {
		return 0, ErrInvalidPath
	}
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	byProject, err := manager.automaticCandidates(ctx)
	if err != nil {
		return 0, err
	}
	var eligible []automaticCandidate
	for _, snapshots := range byProject {
		if len(snapshots) > keep {
			eligible = append(eligible, snapshots[keep:]...)
		}
	}
	return manager.removeAutomaticCandidates(ctx, bytesNeeded, eligible)
}

// ReclaimOldAutomaticForDiskPressure deletes old automatic snapshots while
// protecting the newest automatic snapshot of every project. Manual backups
// are not considered even when no automatic snapshot exists.
func (manager *Manager) ReclaimOldAutomaticForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	byProject, err := manager.automaticCandidates(ctx)
	if err != nil {
		return 0, err
	}
	var eligible []automaticCandidate
	for _, snapshots := range byProject {
		if len(snapshots) > 1 {
			eligible = append(eligible, snapshots[1:]...)
		}
	}
	return manager.removeAutomaticCandidates(ctx, bytesNeeded, eligible)
}

// automaticCandidates returns each project's automatic snapshots newest first.
func (manager *Manager) automaticCandidates(ctx context.Context) (map[string][]automaticCandidate, error) {
	projects, err := os.ReadDir(manager.backupDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]automaticCandidate)
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() || !validSafeID(project.Name()) {
			continue
		}
		projectDir := filepath.Join(manager.backupDir, project.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !validBackupID(entry.Name()) {
				continue
			}
			manifest, err := manager.loadManifest(project.Name(), entry.Name())
			if err != nil {
				return nil, err
			}
			if manifest.Trigger == TriggerManual {
				continue
			}
			path := filepath.Join(projectDir, entry.Name())
			bytes, err := secureLogicalBytes(path)
			if err != nil {
				return nil, err
			}
			result[project.Name()] = append(result[project.Name()], automaticCandidate{
				projectUID: project.Name(), backupID: entry.Name(), createdAt: manifest.CreatedAt, bytes: bytes,
			})
		}
	}
	for project := range result {
		sort.Slice(result[project], func(i, j int) bool {
			left, right := result[project][i], result[project][j]
			if !left.createdAt.Equal(right.createdAt) {
				return left.createdAt.After(right.createdAt)
			}
			return left.backupID > right.backupID
		})
	}
	return result, nil
}

func (manager *Manager) removeAutomaticCandidates(ctx context.Context, bytesNeeded int64, candidates []automaticCandidate) (int64, error) {
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].createdAt.Equal(candidates[j].createdAt) {
			return candidates[i].createdAt.Before(candidates[j].createdAt)
		}
		if candidates[i].projectUID != candidates[j].projectUID {
			return candidates[i].projectUID < candidates[j].projectUID
		}
		return candidates[i].backupID < candidates[j].backupID
	})
	var freed int64
	for _, candidate := range candidates {
		if freed >= bytesNeeded {
			break
		}
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		path := filepath.Join(manager.backupDir, candidate.projectUID, candidate.backupID)
		// Recheck the manifest under the exclusive storage lock immediately
		// before unlink. This is the hard manual-backup protection boundary.
		manifest, err := manager.loadManifest(candidate.projectUID, candidate.backupID)
		if err != nil {
			return freed, err
		}
		if manifest.Trigger == TriggerManual {
			return freed, fmt.Errorf("backup: refusing to auto-delete manual backup %q", candidate.backupID)
		}
		bytes, err := secureLogicalBytes(path)
		if err != nil {
			return freed, err
		}
		if bytes != candidate.bytes {
			return freed, ErrConcurrentModification
		}
		if err := removeSecurePath(path); err != nil {
			return freed, err
		}
		freed += bytes
	}
	return freed, nil
}

func secureLogicalBytes(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, ErrSymlink
	}
	if info.Mode().IsRegular() {
		return info.Size(), nil
	}
	if !info.IsDir() {
		return 0, ErrInvalidArchive
	}
	var total int64
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == path || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrSymlink
		}
		if info.Size() > math.MaxInt64-total {
			return ErrInvalidArchive
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func removeSecurePath(path string) error {
	parent, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || filepath.Clean(name) != name {
		return ErrInvalidPath
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(name); err != nil {
		return err
	}
	return syncDirectory(parent)
}
