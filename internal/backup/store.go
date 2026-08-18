package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type Manager struct {
	stateDir   string
	backupDir  string
	journalDir string
	budget     BudgetAdmitter
	index      MetadataSink
	storageMu  sync.RWMutex

	mu      sync.RWMutex
	blocked map[string]error
	hooks   testHooks
}

type testHooks struct {
	afterReplacement func(int) error
	rollbackFailure  func() error
}

func New(stateDir string, budget BudgetAdmitter, index MetadataSink) (*Manager, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve state directory: %w", err)
	}
	if err := secureDirectory(abs); err != nil {
		return nil, err
	}
	manager := &Manager{
		stateDir: abs, backupDir: filepath.Join(abs, "backups"),
		journalDir: filepath.Join(abs, "restore-journal"), budget: budget, index: index,
		blocked: make(map[string]error),
	}
	if err := secureDirectory(manager.backupDir); err != nil {
		return nil, err
	}
	if err := secureDirectory(manager.journalDir); err != nil {
		return nil, err
	}
	return manager, nil
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("backup: create secure directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("backup: inspect secure directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrSymlink
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("backup: directory %q must be mode 0700", path)
	}
	return nil
}

type openedSource struct {
	rel  string
	file *os.File
	info os.FileInfo
}

func (manager *Manager) Create(ctx context.Context, request CreateRequest) (Backup, error) {
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	return manager.create(ctx, request, false)
}

// List returns only index-safe metadata. In particular, manifests' working
// directories and archive filesystem paths never cross this boundary.
func (manager *Manager) List(ctx context.Context, projectUID string) ([]Metadata, error) {
	manager.storageMu.RLock()
	defer manager.storageMu.RUnlock()
	if !validSafeID(projectUID) {
		return nil, ErrInvalidPath
	}
	projectDir := filepath.Join(manager.backupDir, projectUID)
	entries, err := os.ReadDir(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return []Metadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Metadata, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		backupID := entry.Name()
		if !validBackupID(backupID) || !entry.IsDir() {
			continue
		}
		manifest, err := manager.loadManifest(projectUID, backupID)
		if err != nil {
			return nil, err
		}
		backupPath := filepath.Join(projectDir, backupID)
		archive, err := openSecureRegular(filepath.Join(backupPath, "files.tar.gz"))
		if err != nil {
			return nil, err
		}
		archiveInfo, statErr := archive.Stat()
		closeErr := archive.Close()
		if statErr != nil {
			return nil, statErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		manifestFile, err := openSecureRegular(filepath.Join(backupPath, "manifest.json"))
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		manifestSize, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: manifestFile})
		closeErr = manifestFile.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result = append(result, Metadata{
			BackupID: backupID, ProjectUID: projectUID, CreatedAt: manifest.CreatedAt,
			Trigger: manifest.Trigger, FileCount: len(manifest.Files),
			SizeBytes: archiveInfo.Size() + manifestSize, ManifestSHA256: hex.EncodeToString(hash.Sum(nil)),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].BackupID > result[j].BackupID
	})
	return result, nil
}

func (manager *Manager) create(ctx context.Context, request CreateRequest, allowEmpty bool) (Backup, error) {
	if err := validateProject(request.Project); err != nil {
		return Backup{}, err
	}
	if !validTrigger(request.Trigger) || !validSafeID(request.OperationID) {
		return Backup{}, fmt.Errorf("%w: invalid backup trigger or operation id", ErrInvalidPath)
	}
	if len(request.RelativePaths) == 0 && !allowEmpty {
		return Backup{}, fmt.Errorf("%w: backup has no files", ErrInvalidPath)
	}
	createdAt := request.CreatedAt.UTC().Round(0)
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	sources, _, err := openSources(request.Project.WorkingDir, request.RelativePaths)
	if err != nil {
		return Backup{}, err
	}
	defer closeSources(sources)
	estimated := estimatedBackupBytes(sources)
	if manager.budget != nil {
		if err := manager.budget.AdmitBackup(ctx, Admission{ProjectUID: request.Project.UID, Trigger: request.Trigger, EstimatedBytes: estimated}); err != nil {
			return Backup{}, err
		}
	}
	backupID, err := newBackupID(createdAt)
	if err != nil {
		return Backup{}, err
	}
	projectDir := filepath.Join(manager.backupDir, request.Project.UID)
	if err := secureDirectory(projectDir); err != nil {
		return Backup{}, err
	}
	if err := ctx.Err(); err != nil {
		return Backup{}, err
	}
	staging := filepath.Join(projectDir, "."+backupID+".tmp")
	final := filepath.Join(projectDir, backupID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Backup{}, fmt.Errorf("backup: create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	manifest := Manifest{
		BackupID: backupID, ProjectUID: request.Project.UID, ProjectName: request.Project.Name,
		WorkingDir: request.Project.WorkingDir, CreatedAt: createdAt, Trigger: request.Trigger,
		OperationID: request.OperationID,
	}
	archiveSize, entries, err := writeArchive(ctx, filepath.Join(staging, "files.tar.gz"), sources)
	if err != nil {
		return Backup{}, err
	}
	if err := ctx.Err(); err != nil {
		return Backup{}, err
	}
	manifest.Files = entries
	manifestSize, manifestHash, err := writeJSONFile(filepath.Join(staging, "manifest.json"), manifest)
	if err != nil {
		return Backup{}, err
	}
	if err := syncDirectory(staging); err != nil {
		return Backup{}, err
	}
	if err := ctx.Err(); err != nil {
		return Backup{}, err
	}
	if err := os.Rename(staging, final); err != nil {
		return Backup{}, fmt.Errorf("backup: publish backup: %w", err)
	}
	committed = true
	if err := syncDirectory(projectDir); err != nil {
		return Backup{}, err
	}
	metadata := Metadata{
		BackupID: backupID, ProjectUID: request.Project.UID, CreatedAt: createdAt,
		Trigger: request.Trigger, FileCount: len(entries), SizeBytes: archiveSize + manifestSize,
		ManifestSHA256: manifestHash,
	}
	result := Backup{Manifest: manifest, Metadata: metadata}
	if manager.index != nil {
		if err := manager.index.RecordBackup(ctx, metadata); err != nil {
			return result, fmt.Errorf("backup: local backup committed but metadata indexing failed: %w", err)
		}
	}
	return result, nil
}

func openSources(workingDir string, relativePaths []string) ([]openedSource, int64, error) {
	rootFD, err := syscall.Open(workingDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, 0, ErrSymlink
		}
		return nil, 0, fmt.Errorf("backup: open project directory: %w", err)
	}
	defer syscall.Close(rootFD)
	seen := make(map[string]struct{}, len(relativePaths))
	var sources []openedSource
	var estimated int64
	for _, rel := range relativePaths {
		if !validManagedPath(rel) {
			closeSources(sources)
			return nil, 0, fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		if _, duplicate := seen[rel]; duplicate {
			closeSources(sources)
			return nil, 0, fmt.Errorf("%w: duplicate %q", ErrInvalidPath, rel)
		}
		seen[rel] = struct{}{}
		fd, err := syscall.Openat(rootFD, rel, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			closeSources(sources)
			if err == syscall.ELOOP {
				return nil, 0, fmt.Errorf("%w: %q", ErrSymlink, rel)
			}
			return nil, 0, fmt.Errorf("backup: open %q: %w", rel, err)
		}
		file := os.NewFile(uintptr(fd), rel)
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			closeSources(sources)
			return nil, 0, fmt.Errorf("%w: %q is not a regular file", ErrInvalidPath, rel)
		}
		estimated += info.Size()
		sources = append(sources, openedSource{rel: rel, file: file, info: info})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].rel < sources[j].rel })
	return sources, estimated, nil
}

func closeSources(sources []openedSource) {
	for _, source := range sources {
		_ = source.file.Close()
	}
}

func writeArchive(ctx context.Context, path string, sources []openedSource) (int64, []FileEntry, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, nil, fmt.Errorf("backup: create archive: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := make([]FileEntry, 0, len(sources))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if _, err := source.file.Seek(0, io.SeekStart); err != nil {
			return 0, nil, err
		}
		header := &tar.Header{Name: source.rel, Mode: int64(source.info.Mode().Perm()), Size: source.info.Size(), ModTime: source.info.ModTime(), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			return 0, nil, err
		}
		hash := sha256.New()
		written, err := io.Copy(io.MultiWriter(tarWriter, hash), contextReader{ctx: ctx, reader: source.file})
		if err != nil {
			return 0, nil, fmt.Errorf("backup: read %q: %w", source.rel, err)
		}
		if written != source.info.Size() {
			return 0, nil, fmt.Errorf("%w: %q size changed while reading", ErrConcurrentModification, source.rel)
		}
		after, err := source.file.Stat()
		if err != nil || after.Size() != source.info.Size() || !after.ModTime().Equal(source.info.ModTime()) {
			return 0, nil, fmt.Errorf("%w: %q", ErrConcurrentModification, source.rel)
		}
		entries = append(entries, FileEntry{RelPath: source.rel, SHA256: hex.EncodeToString(hash.Sum(nil)), Mode: uint32(source.info.Mode().Perm()), Size: source.info.Size()})
	}
	if err := tarWriter.Close(); err != nil {
		return 0, nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return 0, nil, err
	}
	if err := file.Sync(); err != nil {
		return 0, nil, err
	}
	if err := file.Close(); err != nil {
		return 0, nil, err
	}
	closed = true
	info, err := os.Stat(path)
	if err != nil {
		return 0, nil, err
	}
	return info.Size(), entries, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func writeJSONFile(path string, value any) (int64, string, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return 0, "", err
	}
	payload = append(payload, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	if err := writeAll(file, payload); err != nil {
		file.Close()
		return 0, "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, "", err
	}
	if err := file.Close(); err != nil {
		return 0, "", err
	}
	hash := sha256.Sum256(payload)
	return int64(len(payload)), hex.EncodeToString(hash[:]), nil
}

func (manager *Manager) LoadManifest(projectUID, backupID string) (Manifest, error) {
	manager.storageMu.RLock()
	defer manager.storageMu.RUnlock()
	return manager.loadManifest(projectUID, backupID)
}

func (manager *Manager) loadManifest(projectUID, backupID string) (Manifest, error) {
	if !validSafeID(projectUID) || !validBackupID(backupID) {
		return Manifest{}, ErrInvalidPath
	}
	path := filepath.Join(manager.backupDir, projectUID, backupID, "manifest.json")
	if err := validateStoredBackupDirectory(filepath.Dir(path)); err != nil {
		return Manifest{}, err
	}
	file, err := openSecureRegular(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrInvalidArchive, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, err
	}
	if manifest.BackupID != backupID || manifest.ProjectUID != projectUID || !validTrigger(manifest.Trigger) ||
		!validSafeID(manifest.OperationID) || !filepath.IsAbs(manifest.WorkingDir) || manifest.ProjectName == "" || manifest.CreatedAt.IsZero() {
		return Manifest{}, ErrInvalidArchive
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, entry := range manifest.Files {
		if !validManagedPath(entry.RelPath) || entry.Size < 0 || len(entry.SHA256) != sha256.Size*2 || entry.Mode&^0o777 != 0 {
			return Manifest{}, ErrInvalidArchive
		}
		if _, duplicate := seen[entry.RelPath]; duplicate {
			return Manifest{}, ErrInvalidArchive
		}
		seen[entry.RelPath] = struct{}{}
	}
	return manifest, nil
}

func (manager *Manager) PruneAutomatic(projectUID string, keep int) ([]string, error) {
	manager.storageMu.Lock()
	defer manager.storageMu.Unlock()
	if !validSafeID(projectUID) || keep < 1 {
		return nil, ErrInvalidPath
	}
	projectDir := filepath.Join(manager.backupDir, projectUID)
	entries, err := os.ReadDir(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id string
		at time.Time
	}
	var automatic []candidate
	for _, entry := range entries {
		if !entry.IsDir() || !validBackupID(entry.Name()) {
			continue
		}
		manifest, err := manager.loadManifest(projectUID, entry.Name())
		if err != nil {
			return nil, err
		}
		if manifest.Trigger != TriggerManual {
			automatic = append(automatic, candidate{manifest.BackupID, manifest.CreatedAt})
		}
	}
	sort.Slice(automatic, func(i, j int) bool { return automatic[i].at.After(automatic[j].at) })
	if len(automatic) <= keep {
		return nil, nil
	}
	var deleted []string
	for _, candidate := range automatic[keep:] {
		if err := os.RemoveAll(filepath.Join(projectDir, candidate.id)); err != nil {
			return deleted, err
		}
		deleted = append(deleted, candidate.id)
	}
	if len(deleted) > 0 {
		if err := syncDirectory(projectDir); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func validateProject(project Project) error {
	if !validSafeID(project.UID) || project.Name == "" || len(project.Name) > 255 || strings.ContainsRune(project.Name, 0) ||
		project.WorkingDir == "" || !filepath.IsAbs(project.WorkingDir) || filepath.Clean(project.WorkingDir) != project.WorkingDir || strings.ContainsRune(project.WorkingDir, 0) {
		return ErrInvalidPath
	}
	resolved, err := filepath.EvalSymlinks(project.WorkingDir)
	if err != nil {
		return err
	}
	if resolved != project.WorkingDir {
		return ErrSymlink
	}
	info, err := os.Lstat(project.WorkingDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrSymlink
	}
	if !info.IsDir() {
		return ErrInvalidPath
	}
	return nil
}

func validManagedPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || strings.Contains(path, string(filepath.Separator)) {
		return false
	}
	if path == ".env" || (strings.HasPrefix(path, ".env.") && len(path) > len(".env.")) {
		return true
	}
	switch path {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
		return true
	}
	if strings.HasPrefix(path, "compose.override.") || strings.HasPrefix(path, "docker-compose.override.") {
		return strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
	}
	return strings.HasPrefix(path, "compose.") && strings.HasSuffix(path, ".yaml") && len(path) > len("compose..yaml")
}

func validTrigger(trigger Trigger) bool {
	return trigger == TriggerManual || trigger == TriggerPreWrite || trigger == TriggerPreRestore
}

func validSafeID(id string) bool {
	return safeID.MatchString(id) && id != "." && id != ".."
}

func estimatedBackupBytes(sources []openedSource) int64 {
	// Tar uses one 512-byte header plus 512-byte padding per file and a
	// 1024-byte trailer. Gzip's stored-block overhead is well below 1%; keep a
	// conservative 1 MiB allowance for gzip and manifest metadata.
	tarBytes := int64(1024)
	for _, source := range sources {
		tarBytes += 512 + ((source.info.Size()+511)/512)*512
	}
	return tarBytes + tarBytes/100 + (1 << 20)
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func newBackupID(at time.Time) (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return at.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(randomBytes[:]), nil
}

func validBackupID(id string) bool {
	if len(id) != len("20060102T150405.000000000Z-")+16 {
		return false
	}
	_, err := time.Parse("20060102T150405.000000000Z", id[:len(id)-17])
	if err != nil || id[len(id)-17] != '-' {
		return false
	}
	_, err = hex.DecodeString(id[len(id)-16:])
	return err == nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing manifest data", ErrInvalidArchive)
	}
	return nil
}
