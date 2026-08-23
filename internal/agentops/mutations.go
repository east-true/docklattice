package agentops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/operation"
	"github.com/east-true/dockpilot/internal/safefile"
)

const (
	operationPayloadMaxBytes = 1 << 20
	operationPathMaxBytes    = 1024
	backupPathMaxCount       = 64
)

var safeOpaqueID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type BackupManager interface {
	Create(context.Context, backup.CreateRequest) (backup.Backup, error)
	Restore(context.Context, backup.RestoreRequest) (backup.RestoreResult, error)
	CheckChangeAllowed(string) error
	PruneAutomatic(string, int) ([]string, error)
}

// DiskAdmitter connects the coordinator to the independently-owned
// diskbudget state/policy evaluator.
type DiskAdmitter interface {
	AdmitOperation(context.Context, diskbudget.Operation) error
	AdmitProjectStaging(context.Context, int64, int64, int64) error
}

type DiskAdmitterFunc func(context.Context, diskbudget.Operation) error

func (function DiskAdmitterFunc) AdmitOperation(ctx context.Context, kind diskbudget.Operation) error {
	return function(ctx, kind)
}

func (DiskAdmitterFunc) AdmitProjectStaging(context.Context, int64, int64, int64) error { return nil }

type operationCommand struct {
	fileWrite       *fileWriteCommand
	backupCreate    *backupCreateCommand
	backupID        string
	composeServices []string
}

type fileWriteCommand struct {
	relativePath   string
	expectedSHA256 string
	content        []byte
}

type backupCreateCommand struct{ relativePaths []string }

type versionPayload struct {
	Version int `json:"version"`
}

type fileWritePayload struct {
	Version        int    `json:"version"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Content        string `json:"content"`
}

type backupCreatePayload struct {
	Version       int      `json:"version"`
	RelativePaths []string `json:"relative_paths"`
}

func parseOperationCommand(kind operation.Type, target string, payload []byte) (operationCommand, error) {
	if len(payload) > operationPayloadMaxBytes {
		return operationCommand{}, fmt.Errorf("agentops: operation payload exceeds %d bytes", operationPayloadMaxBytes)
	}
	switch {
	case isFileWriteOperation(kind):
		if err := validRelativePath(target); err != nil {
			return operationCommand{}, err
		}
		if !pathMatchesWriteType(kind, target) {
			return operationCommand{}, fmt.Errorf("agentops: relative_path is not valid for %s", kind)
		}
		var decoded fileWritePayload
		if err := decodeStrictPayload(payload, &decoded); err != nil {
			return operationCommand{}, err
		}
		if decoded.Version != 1 || len(decoded.Content) > int(safefile.MaxFileSize) || !validSHA256(decoded.ExpectedSHA256) {
			return operationCommand{}, errors.New("agentops: invalid file write payload")
		}
		return operationCommand{fileWrite: &fileWriteCommand{
			relativePath: target, expectedSHA256: decoded.ExpectedSHA256, content: []byte(decoded.Content),
		}}, nil
	case kind == operation.TypeBackupCreate:
		if target != "" {
			return operationCommand{}, errors.New("agentops: backup.create target must be empty")
		}
		var decoded backupCreatePayload
		if err := decodeStrictPayload(payload, &decoded); err != nil {
			return operationCommand{}, err
		}
		if decoded.Version != 1 || len(decoded.RelativePaths) == 0 || len(decoded.RelativePaths) > backupPathMaxCount {
			return operationCommand{}, errors.New("agentops: invalid backup.create payload")
		}
		seen := make(map[string]struct{}, len(decoded.RelativePaths))
		for _, path := range decoded.RelativePaths {
			if err := validRelativePath(path); err != nil {
				return operationCommand{}, err
			}
			if !pathMatchesWriteType(operation.TypeComposeFileWrite, path) &&
				!pathMatchesWriteType(operation.TypeEnvWrite, path) &&
				!pathMatchesWriteType(operation.TypeOverrideWrite, path) {
				return operationCommand{}, errors.New("agentops: backup relative_path is not a managed project file")
			}
			if _, exists := seen[path]; exists {
				return operationCommand{}, errors.New("agentops: duplicate backup relative_path")
			}
			seen[path] = struct{}{}
		}
		return operationCommand{backupCreate: &backupCreateCommand{relativePaths: append([]string(nil), decoded.RelativePaths...)}}, nil
	case kind == operation.TypeBackupRestore:
		if !safeOpaqueID.MatchString(target) || target == "." || target == ".." {
			return operationCommand{}, errors.New("agentops: invalid backup_id")
		}
		var decoded versionPayload
		if err := decodeStrictPayload(payload, &decoded); err != nil || decoded.Version != 1 {
			return operationCommand{}, errors.New("agentops: invalid backup.restore payload")
		}
		return operationCommand{backupID: target}, nil
	default:
		if len(payload) != 0 {
			return operationCommand{}, fmt.Errorf("agentops: structured options are not supported for %s", kind)
		}
		return operationCommand{}, nil
	}
}

func decodeStrictPayload(payload []byte, destination any) error {
	if len(payload) == 0 {
		return errors.New("agentops: JSON payload is required")
	}
	if !utf8.Valid(payload) {
		return errors.New("agentops: JSON payload is not valid UTF-8")
	}
	if err := rejectDuplicateFields(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("agentops: invalid JSON payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agentops: trailing JSON payload")
	}
	return nil
}

func rejectDuplicateFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("agentops: invalid JSON payload: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("agentops: JSON payload must be one object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("agentops: invalid JSON payload: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("agentops: JSON object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("agentops: duplicate JSON field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("agentops: invalid JSON payload: %w", err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("agentops: invalid JSON payload: %w", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("agentops: JSON payload must be one object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agentops: trailing JSON payload")
	}
	return nil
}

func validRelativePath(path string) error {
	if path == "" || len(path) > operationPathMaxBytes || strings.IndexByte(path, 0) >= 0 || filepath.IsAbs(path) ||
		filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return errors.New("agentops: invalid relative_path")
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return errors.New("agentops: relative_path escapes project")
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func pathMatchesWriteType(kind operation.Type, path string) bool {
	if strings.Contains(path, "/") {
		return false
	}
	switch kind {
	case operation.TypeEnvWrite:
		return path == ".env" || strings.HasPrefix(path, ".env.") && len(path) > len(".env.")
	case operation.TypeOverrideWrite:
		return (strings.HasPrefix(path, "compose.override.") || strings.HasPrefix(path, "docker-compose.override.")) &&
			(strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml"))
	case operation.TypeComposeFileWrite:
		switch path {
		case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
			return true
		default:
			return strings.HasPrefix(path, "compose.") && strings.HasSuffix(path, ".yaml") && !strings.HasPrefix(path, "compose.override.")
		}
	default:
		return false
	}
}

func isFileWriteOperation(kind operation.Type) bool {
	return kind == operation.TypeComposeFileWrite || kind == operation.TypeEnvWrite || kind == operation.TypeOverrideWrite
}

func isBackupOperation(kind operation.Type) bool {
	return kind == operation.TypeBackupCreate || kind == operation.TypeBackupRestore
}

func (s *Service) runFileWrite(ctx context.Context, current *operation.Operation, project composeexec.Project, approvedFiles []safefile.ApprovedFile, command fileWriteCommand) (string, error) {
	defer func() {
		for index := range command.content {
			command.content[index] = 0
		}
	}()
	if err := s.config.Backups.CheckChangeAllowed(current.Snapshot().ProjectKey); err != nil {
		return "", err
	}
	root, err := safefile.OpenRoot(project.WorkingDir, approvedFiles)
	if err != nil {
		return "", err
	}
	defer root.Close()
	total, free, err := root.FilesystemSpace(ctx)
	if err != nil {
		return "", err
	}
	if err := s.config.Admission.AdmitProjectStaging(ctx, total, free, int64(len(command.content))); err != nil {
		return "", err
	}
	result, err := root.Write(ctx, safefile.WriteRequest{
		RelativePath: command.relativePath, ExpectedSHA256: command.expectedSHA256,
		Content: append([]byte(nil), command.content...),
		Validate: func(validateCtx context.Context, input safefile.ValidationInput) error {
			return s.validateComposeFile(validateCtx, project, current.Snapshot().Type, input)
		},
		Snapshot: func(snapshotCtx context.Context, input safefile.SnapshotInput) error {
			_, err := s.config.Backups.Create(snapshotCtx, backup.CreateRequest{
				Project:       backup.Project{UID: current.Snapshot().ProjectKey, Name: project.Name, WorkingDir: project.WorkingDir},
				RelativePaths: []string{input.RelativePath}, Trigger: backup.TriggerPreWrite,
				OperationID: current.Snapshot().OperationID, CreatedAt: time.Now().UTC(),
			})
			if err != nil {
				return err
			}
			_, err = s.config.Backups.PruneAutomatic(current.Snapshot().ProjectKey, backup.AutomaticSnapshotRetention)
			return err
		},
		Commit: func(context.Context) error { return current.EnterCommit() },
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("file %s written sha256=%s", result.RelativePath, result.SHA256), nil
}

func (s *Service) validateComposeFile(ctx context.Context, project composeexec.Project, kind operation.Type, input safefile.ValidationInput) error {
	if input.ProjectRoot != project.WorkingDir {
		return errors.New("agentops: staged validation project mismatch")
	}
	validationProject := project
	validationProject.Files = append([]string(nil), project.Files...)
	if kind == operation.TypeEnvWrite {
		validationProject.EnvFile = input.StagedPath
	} else {
		target := filepath.Clean(filepath.Join(project.WorkingDir, filepath.FromSlash(input.RelativePath)))
		replaced := false
		for index, file := range validationProject.Files {
			if filepath.Clean(file) == target {
				validationProject.Files[index] = input.StagedPath
				replaced = true
			}
		}
		if !replaced {
			if kind != operation.TypeOverrideWrite {
				return errors.New("agentops: compose file is not part of the discovered project")
			}
			validationProject.Files = append(validationProject.Files, input.StagedPath)
		}
	}
	relay := make(chan composeexec.OutputChunk, 16)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range relay {
		}
	}()
	result, runErr := s.config.Compose.Run(ctx, composeexec.Spec{
		Operation: composeexec.OperationConfig, Project: validationProject,
		Flags: composeexec.Flags{ConfigOutput: composeexec.ConfigOutputQuiet},
	}, relay)
	close(relay)
	<-drained
	if runErr != nil {
		return runErr
	}
	if !result.Success() {
		return fmt.Errorf("docker compose config exited with status %d", result.ExitCode)
	}
	return nil
}

func (s *Service) runBackupCreate(ctx context.Context, current *operation.Operation, project composeexec.Project, command backupCreateCommand) (string, error) {
	projectUID := current.Snapshot().ProjectKey
	if err := s.config.Backups.CheckChangeAllowed(projectUID); err != nil {
		return "", err
	}
	created, err := s.config.Backups.Create(ctx, backup.CreateRequest{
		Project:       backup.Project{UID: projectUID, Name: project.Name, WorkingDir: project.WorkingDir},
		RelativePaths: append([]string(nil), command.relativePaths...), Trigger: backup.TriggerManual,
		OperationID: current.Snapshot().OperationID, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("backup %s created files=%d manifest_sha256=%s",
		created.Metadata.BackupID, created.Metadata.FileCount, created.Metadata.ManifestSHA256), nil
}

func (s *Service) runBackupRestore(ctx context.Context, current *operation.Operation, project composeexec.Project, backupID string) (string, error) {
	projectUID := current.Snapshot().ProjectKey
	result, err := s.config.Backups.Restore(ctx, backup.RestoreRequest{
		Project:  backup.Project{UID: projectUID, Name: project.Name, WorkingDir: project.WorkingDir},
		BackupID: backupID, OperationID: current.Snapshot().OperationID, Now: time.Now().UTC(),
		CommitGate: backup.CommitGateFunc(func(context.Context) error { return current.EnterCommit() }),
		Progress:   func(_, _ int) error { return current.AdvanceProgress() },
	})
	if result.PreRestoreSnapshotID != "" {
		if _, pruneErr := s.config.Backups.PruneAutomatic(projectUID, backup.AutomaticSnapshotRetention); pruneErr != nil {
			err = errors.Join(err, pruneErr)
		}
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("backup %s restored files=%d pre_restore_snapshot=%s",
		backupID, result.RestoredFiles, result.PreRestoreSnapshotID), nil
}

func diskOperation(kind operation.Type) (diskbudget.Operation, bool) {
	switch kind {
	case operation.TypeComposePull:
		return diskbudget.OperationComposePull, true
	case operation.TypeComposeUp:
		return diskbudget.OperationComposeUp, true
	case operation.TypeComposeDown:
		return diskbudget.OperationComposeDown, true
	case operation.TypeComposeStart:
		return diskbudget.OperationComposeStart, true
	case operation.TypeComposeStop:
		return diskbudget.OperationComposeStop, true
	case operation.TypeComposeRestart:
		return diskbudget.OperationComposeRestart, true
	case operation.TypeContainerStart:
		return diskbudget.OperationContainerStart, true
	case operation.TypeContainerStop:
		return diskbudget.OperationContainerStop, true
	case operation.TypeContainerRestart:
		return diskbudget.OperationContainerRestart, true
	case operation.TypeContainerRemove:
		return diskbudget.OperationContainerRemove, true
	case operation.TypeComposeFileWrite, operation.TypeEnvWrite, operation.TypeOverrideWrite:
		return diskbudget.OperationFileWrite, true
	case operation.TypeBackupCreate:
		return diskbudget.OperationBackupCreate, true
	case operation.TypeBackupRestore:
		return diskbudget.OperationBackupRestore, true
	default:
		return "", false
	}
}
