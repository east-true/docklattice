package serverapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/servermatrix"
	"github.com/east-true/dockpilot/internal/serverstore"
	"github.com/east-true/dockpilot/internal/webui"
	"google.golang.org/grpc/status"
)

const (
	QueryProjectEnvironment = "project_environment"
	QueryProjectFile        = "file.read"
	QueryProjectBackups     = "backup.list"
	QueryComposePS          = "compose.ps"
	QueryComposeConfig      = "compose.config"
	maxProjectFileBytes     = 1 << 20
	maxComposeOutputBytes   = 128 << 10
	// Reserve room for OperationRequest's IDs, target, type and protobuf
	// framing inside producttransport's 1 MiB message limit.
	maxOperationPayloadBytes = producttransport.DefaultMaxMessageBytes - 4096
	maxOperationOutputBytes  = 64 << 10
	maxConcurrentHostProbes  = 8
	// hostProbeTimeout bounds the live heartbeat the dashboard performs per
	// host. It matches the Server's own liveness loop, which has always
	// bounded its heartbeat at the same value.
	hostProbeTimeout = producttransport.DefaultHeartbeatTimeout
)

type Backend struct {
	store              *serverstore.Store
	registry           *producttransport.SessionRegistry
	liveMu             sync.RWMutex
	liveStats          map[liveStatsKey]liveStatsState
	operationMergeGate chan struct{}
	auditArchiveID     string
	audit              *auditstore.Store
	matrix             *servermatrix.Hub
}

type Option func(*Backend) error

// WithAuditReadModel shares the canonical audit Store with auditsync. Sharing
// is required because ACK stall evidence intentionally includes process-local
// observations that must not be reconstructed from database timestamps.
func WithAuditReadModel(archiveID string, store *auditstore.Store) Option {
	return func(backend *Backend) error {
		if archiveID == "" || len(archiveID) > 256 || !utf8.ValidString(archiveID) || strings.ContainsRune(archiveID, 0) || store == nil {
			return errors.New("serverapi: valid audit read model is required")
		}
		if backend.audit != nil || backend.auditArchiveID != "" {
			return errors.New("serverapi: audit read model is already configured")
		}
		backend.auditArchiveID, backend.audit = archiveID, store
		return nil
	}
}

type liveStatsKey struct{ agentID, containerID string }
type liveStatsState struct {
	viewers int
	latest  webui.StatsSample
	has     bool
}

var (
	canonicalContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
	canonicalSHA256      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	safeOpaqueID         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	composeServiceName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

var _ webui.Backend = (*Backend)(nil)

func New(store *serverstore.Store, registry *producttransport.SessionRegistry, options ...Option) (*Backend, error) {
	if store == nil || store.DB() == nil {
		return nil, errors.New("serverapi: Server store is required")
	}
	if registry == nil {
		return nil, errors.New("serverapi: session registry is required")
	}
	backend := &Backend{
		store: store, registry: registry, liveStats: make(map[liveStatsKey]liveStatsState),
		operationMergeGate: make(chan struct{}, 1),
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("serverapi: nil Backend option")
		}
		if err := option(backend); err != nil {
			return nil, err
		}
	}
	matrix, err := servermatrix.New(servermatrix.Config{
		Sessions: matrixSessions{backend: backend}, Context: matrixContext{backend: backend},
	})
	if err != nil {
		return nil, err
	}
	backend.matrix = matrix
	return backend, nil
}

type agentRow struct {
	id           string
	displayName  string
	capabilities storedCapabilities
	projectScan  *webui.ProjectScan
}

type storedCapabilities struct {
	FSRead        bool   `json:"fs_read"`
	FSWrite       bool   `json:"fs_write"`
	FSReadReason  string `json:"fs_read_reason,omitempty"`
	FSWriteReason string `json:"fs_write_reason,omitempty"`
}

type projectFlags struct {
	Managed                 bool                   `json:"managed"`
	UnmanagedReason         string                 `json:"unmanaged_reason,omitempty"`
	ContainerIDs            []string               `json:"container_ids,omitempty"`
	Services                []string               `json:"services,omitempty"`
	IncludedBy              []string               `json:"included_by,omitempty"`
	SourceReferences        []agentSourceReference `json:"source_references,omitempty"`
	SourceGraphComplete     bool                   `json:"source_graph_complete,omitempty"`
	IncludedWorkDirs        []string               `json:"included_work_dirs,omitempty"`
	ReadOnly                bool                   `json:"read_only"`
	Collision               bool                   `json:"collision"`
	Missing                 bool                   `json:"missing,omitempty"`
	Stale                   bool                   `json:"stale,omitempty"`
	ComposeExecutable       bool                   `json:"compose_executable,omitempty"`
	FilesystemWritable      bool                   `json:"filesystem_writable,omitempty"`
	RestoreRecoveryRequired bool                   `json:"restore_recovery_required,omitempty"`
	CapabilityReason        string                 `json:"capability_reason,omitempty"`
	CurrentFingerprint      string                 `json:"current_fingerprint,omitempty"`
	LastVerifiedFingerprint string                 `json:"last_verified_fingerprint,omitempty"`
	LastVerifiedAt          string                 `json:"last_verified_at,omitempty"`
	LastObservedAt          string                 `json:"last_observed_at,omitempty"`
	Drift                   string                 `json:"drift,omitempty"`
}

func (b *Backend) Dashboard(ctx context.Context) (webui.Dashboard, error) {
	agents, err := b.loadAgents(ctx)
	if err != nil {
		return webui.Dashboard{}, err
	}
	reconciliation, err := b.reconcileDashboardAgents(ctx, agents)
	if err != nil {
		return webui.Dashboard{}, err
	}
	// Reload the small durable Agent cache so the response includes the scan
	// status committed by reconciliation rather than the pre-query watermark.
	agents, err = b.loadAgents(ctx)
	if err != nil {
		return webui.Dashboard{}, err
	}
	projects, err := b.loadProjects(ctx)
	if err != nil {
		return webui.Dashboard{}, err
	}
	dashboard := webui.Dashboard{
		Hosts:    make([]webui.Host, len(agents)),
		Projects: projects,
	}
	// Heartbeats are independent live reads. A fixed worker count avoids both
	// serial head-of-line blocking and attacker-amplified goroutine fan-out.
	jobs := make(chan int)
	var wait sync.WaitGroup
	workers := min(len(agents), maxConcurrentHostProbes)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				dashboard.Hosts[index] = b.liveHost(ctx, agents[index])
			}
		}()
	}
	for index := range agents {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for index := range dashboard.Hosts {
		if reason, failed := reconciliation.discoveryReasons[dashboard.Hosts[index].ID]; failed {
			dashboard.Hosts[index].Capabilities.Discovery = webui.Capability{Reason: reason}
		} else if dashboard.Hosts[index].State == string(producttransport.StateActive) {
			if scan := dashboard.Hosts[index].ProjectScan; scan != nil && scan.Truncated {
				dashboard.Hosts[index].Capabilities.Discovery = webui.Capability{Reason: "project discovery scan was truncated: " + scan.StopReason}
			} else {
				dashboard.Hosts[index].Capabilities.Discovery = webui.Capability{Enabled: true}
			}
		}
		if reason, failed := reconciliation.operationRecoveryReason[dashboard.Hosts[index].ID]; failed {
			dashboard.Hosts[index].Capabilities.OperationRecovery = webui.Capability{Reason: reason}
		} else if dashboard.Hosts[index].State == string(producttransport.StateActive) {
			dashboard.Hosts[index].Capabilities.OperationRecovery = webui.Capability{Enabled: true}
		}
	}
	return dashboard, nil
}

func (b *Backend) Host(ctx context.Context, agentID string) (webui.Host, error) {
	if agentID == "" {
		return webui.Host{}, fmt.Errorf("%w: Agent ID is required", webui.ErrInvalidRequest)
	}
	agent, err := b.loadAgent(ctx, agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return webui.Host{}, fmt.Errorf("%w: Agent %q", webui.ErrNotFound, agentID)
	}
	if err != nil {
		return webui.Host{}, err
	}
	return b.liveHost(ctx, agent), nil
}

func (b *Backend) ProjectEnvironment(ctx context.Context, projectUID string) ([]webui.EnvironmentEntry, error) {
	if projectUID == "" {
		return nil, fmt.Errorf("%w: project UID is required", webui.ErrInvalidRequest)
	}
	access, err := b.projectAccess(ctx, projectUID, projectRead)
	if err != nil {
		return nil, err
	}
	if !access.capabilities.FSRead {
		return nil, fmt.Errorf("%w: filesystem read capability is unavailable", webui.ErrUnavailable)
	}
	session, err := b.activeSession(access.agentID)
	if err != nil {
		return nil, err
	}
	response, err := session.Query(ctx, producttransport.QueryRequest{Kind: QueryProjectEnvironment, Target: projectUID})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: access.agentID, action: "environment query", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return nil, &corruptDataError{boundary: "Agent environment response", cause: errors.New("payload exceeds transport limit")}
	}
	var entries []webui.EnvironmentEntry
	if err := decodeStrictJSON(response.Payload, &entries); err != nil {
		return nil, &corruptDataError{boundary: "Agent environment response", cause: err}
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Name == "" || !utf8.ValidString(entry.Name) || !utf8.ValidString(entry.Value) || !entry.Secret {
			return nil, &corruptDataError{boundary: "Agent environment response", cause: errors.New("invalid, unmarked-secret environment entry")}
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return nil, &corruptDataError{boundary: "Agent environment response", cause: fmt.Errorf("duplicate variable %q", entry.Name)}
		}
		seen[entry.Name] = struct{}{}
	}
	return entries, nil
}

type projectFilePayload struct {
	RelativePath string `json:"relative_path"`
}

type agentProjectFile struct {
	RelativePath string `json:"relative_path"`
	Content      string `json:"content"`
	SHA256       string `json:"sha256"`
	MTime        string `json:"mtime"`
	Mode         uint32 `json:"mode"`
	LineEndings  string `json:"line_endings"`
	Secret       bool   `json:"secret"`
}

type agentBackup struct {
	BackupID       string `json:"backup_id"`
	ProjectUID     string `json:"project_uid"`
	CreatedAt      string `json:"created_at"`
	Trigger        string `json:"trigger"`
	FileCount      int    `json:"file_count"`
	SizeBytes      int64  `json:"size_bytes"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

func (b *Backend) ProjectFile(ctx context.Context, projectUID, relativePath string) (webui.ProjectFile, error) {
	if !validManagedPath(relativePath) {
		return webui.ProjectFile{}, fmt.Errorf("%w: a managed project relative_path is required", webui.ErrInvalidRequest)
	}
	access, err := b.projectAccess(ctx, projectUID, projectRead)
	if err != nil {
		return webui.ProjectFile{}, err
	}
	if !access.capabilities.FSRead {
		reason := access.capabilities.FSReadReason
		if reason == "" {
			reason = "filesystem read capability is unavailable"
		}
		return webui.ProjectFile{}, fmt.Errorf("%w: %s", webui.ErrUnavailable, reason)
	}
	session, err := b.activeSession(access.agentID)
	if err != nil {
		return webui.ProjectFile{}, err
	}
	payload, err := json.Marshal(projectFilePayload{RelativePath: relativePath})
	if err != nil {
		return webui.ProjectFile{}, fmt.Errorf("serverapi: encode file query: %w", err)
	}
	defer clear(payload)
	response, err := session.Query(ctx, producttransport.QueryRequest{Kind: QueryProjectFile, Target: projectUID, Payload: payload})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return webui.ProjectFile{}, ctx.Err()
		}
		if status.Convert(err).Message() == "Agent query response exceeds 1 MiB" {
			return webui.ProjectFile{}, fmt.Errorf("%w: project file cannot fit the 1 MiB Agent transport frame", webui.ErrTooLarge)
		}
		return webui.ProjectFile{}, &liveUnavailableError{agentID: access.agentID, action: "project file query", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return webui.ProjectFile{}, &corruptDataError{boundary: "Agent file response", cause: errors.New("payload exceeds transport limit")}
	}
	var value agentProjectFile
	if err := decodeStrictJSON(response.Payload, &value); err != nil {
		return webui.ProjectFile{}, &corruptDataError{boundary: "Agent file response", cause: err}
	}
	mtime, err := time.Parse(time.RFC3339Nano, value.MTime)
	if err != nil || mtime.IsZero() || value.RelativePath != relativePath || !utf8.ValidString(value.Content) ||
		!canonicalSHA256.MatchString(value.SHA256) || len(value.Content) > maxProjectFileBytes || value.Mode > 0o7777 ||
		!validLineEndings(value.LineEndings) || isEnvironmentPath(relativePath) && !value.Secret {
		return webui.ProjectFile{}, &corruptDataError{boundary: "Agent file response", cause: errors.New("invalid file metadata or content")}
	}
	return webui.ProjectFile{
		RelativePath: value.RelativePath, Content: value.Content, SHA256: value.SHA256, MTime: mtime,
		Mode: value.Mode, LineEndings: value.LineEndings, Secret: value.Secret,
	}, nil
}

func (b *Backend) ProjectBackups(ctx context.Context, projectUID string) ([]webui.Backup, error) {
	access, err := b.projectAccess(ctx, projectUID, projectRead)
	if err != nil {
		return nil, err
	}
	session, err := b.activeSession(access.agentID)
	if err != nil {
		return nil, err
	}
	response, err := session.Query(ctx, producttransport.QueryRequest{Kind: QueryProjectBackups, Target: projectUID})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: access.agentID, action: "backup list query", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return nil, &corruptDataError{boundary: "Agent backup response", cause: errors.New("payload exceeds transport limit")}
	}
	var decoded []agentBackup
	if err := decodeStrictJSON(response.Payload, &decoded); err != nil {
		return nil, &corruptDataError{boundary: "Agent backup response", cause: err}
	}
	result := make([]webui.Backup, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	for index, item := range decoded {
		createdAt, parseErr := time.Parse(time.RFC3339Nano, item.CreatedAt)
		_, duplicate := seen[item.BackupID]
		if parseErr != nil || createdAt.IsZero() || !validOpaqueID(item.BackupID) || item.ProjectUID != projectUID ||
			!validBackupTrigger(item.Trigger) || item.FileCount < 0 || item.SizeBytes < 0 ||
			!canonicalSHA256.MatchString(item.ManifestSHA256) || duplicate {
			return nil, &corruptDataError{boundary: "Agent backup response", cause: errors.New("invalid or duplicate backup metadata")}
		}
		seen[item.BackupID] = struct{}{}
		result[index] = webui.Backup{
			ID: item.BackupID, ProjectUID: item.ProjectUID, CreatedAt: createdAt, Trigger: item.Trigger,
			FileCount: item.FileCount, SizeBytes: item.SizeBytes, ManifestSHA256: item.ManifestSHA256,
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	if err := b.syncBackupIndex(ctx, access.agentID, projectUID, result); err != nil {
		return nil, err
	}
	return result, nil
}

// classifyStoreBusy maps SQLite write contention onto the transient answer the
// API contract already has. A write transaction that waited out busy_timeout
// and still could not take the lock is load; reporting it as a Server
// invariant failure would be wrong, and the browser would be told to treat a
// retryable condition as a bug.
func classifyStoreBusy(err error) error {
	if err == nil || !serverstore.Busy(err) {
		return err
	}
	return fmt.Errorf("%w: the Server database is busy", webui.ErrBusy)
}

func (b *Backend) syncBackupIndex(ctx context.Context, agentID, projectUID string, backups []webui.Backup) (err error) {
	defer func() { err = classifyStoreBusy(err) }()
	tx, err := b.store.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("serverapi: begin backup metadata sync: %w", err)
	}
	defer tx.Rollback()
	seen := make(map[string]struct{}, len(backups))
	for _, item := range backups {
		flags, marshalErr := json.Marshal(struct {
			Trigger   string `json:"trigger"`
			FileCount int    `json:"file_count"`
		}{Trigger: item.Trigger, FileCount: item.FileCount})
		if marshalErr != nil {
			return fmt.Errorf("serverapi: encode backup metadata flags: %w", marshalErr)
		}
		result, execErr := tx.ExecContext(ctx, `
			INSERT INTO backup_index(
				id, agent_id, project_uid, kind, created_at, size_bytes,
				storage_path, manifest_sha256, flags_json
			) VALUES(?, ?, ?, 'configuration', ?, ?, '', ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				project_uid = excluded.project_uid,
				kind = excluded.kind,
				created_at = excluded.created_at,
				size_bytes = excluded.size_bytes,
				storage_path = excluded.storage_path,
				manifest_sha256 = excluded.manifest_sha256,
				flags_json = excluded.flags_json
			WHERE backup_index.agent_id = excluded.agent_id
			  AND backup_index.project_uid = excluded.project_uid
		`, item.ID, agentID, projectUID, item.CreatedAt.UTC().Format(time.RFC3339Nano), item.SizeBytes, item.ManifestSHA256, string(flags))
		clear(flags)
		if execErr != nil {
			return fmt.Errorf("serverapi: index backup metadata: %w", execErr)
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil || affected != 1 {
			return &corruptDataError{boundary: "backup_index.id", cause: errors.New("backup ID belongs to a different Agent")}
		}
		seen[item.ID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM backup_index WHERE agent_id = ? AND project_uid = ?`, agentID, projectUID)
	if err != nil {
		return fmt.Errorf("serverapi: query stale backup metadata: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("serverapi: scan stale backup metadata: %w", err)
		}
		if _, current := seen[id]; !current {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("serverapi: close backup metadata rows: %w", err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup_index WHERE id = ? AND agent_id = ? AND project_uid = ?`, id, agentID, projectUID); err != nil {
			return fmt.Errorf("serverapi: remove stale backup metadata: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("serverapi: commit backup metadata sync: %w", err)
	}
	return nil
}

func (b *Backend) WriteProjectFile(ctx context.Context, request webui.FileWriteRequest) (webui.Operation, error) {
	if !validOperationID(request.ID) || !validManagedPath(request.RelativePath) ||
		!canonicalSHA256.MatchString(request.ExpectedSHA256) || !utf8.ValidString(request.Content) || len(request.Content) > maxProjectFileBytes {
		return webui.Operation{}, fmt.Errorf("%w: invalid file write request", webui.ErrInvalidRequest)
	}
	kind, ok := fileWriteKind(request.RelativePath)
	if !ok {
		return webui.Operation{}, fmt.Errorf("%w: file is not writable by a v1 file operation", webui.ErrInvalidRequest)
	}
	access, err := b.projectAccess(ctx, request.ProjectUID, projectMutate)
	if err != nil {
		return webui.Operation{}, err
	}
	if !access.capabilities.FSWrite {
		reason := access.capabilities.FSWriteReason
		if reason == "" {
			reason = "filesystem write capability is unavailable"
		}
		return webui.Operation{}, fmt.Errorf("%w: %s", webui.ErrUnavailable, reason)
	}
	payload, err := json.Marshal(struct {
		Version        int    `json:"version"`
		ExpectedSHA256 string `json:"expected_sha256"`
		Content        string `json:"content"`
	}{Version: 1, ExpectedSHA256: request.ExpectedSHA256, Content: request.Content})
	if err != nil {
		return webui.Operation{}, fmt.Errorf("serverapi: encode file operation: %w", err)
	}
	defer clear(payload)
	if len(payload) > maxOperationPayloadBytes {
		return webui.Operation{}, fmt.Errorf("%w: encoded file cannot fit the 1 MiB Agent transport frame", webui.ErrTooLarge)
	}
	return b.dispatchOperation(ctx, access.agentID, request.ID, request.ProjectUID, kind, request.RelativePath, payload)
}

func (b *Backend) CreateBackup(ctx context.Context, request webui.BackupCreateRequest) (webui.Operation, error) {
	if !validOperationID(request.ID) || len(request.RelativePaths) == 0 || len(request.RelativePaths) > 64 {
		return webui.Operation{}, fmt.Errorf("%w: operation_id and 1..64 relative_paths are required", webui.ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(request.RelativePaths))
	for _, path := range request.RelativePaths {
		if !validManagedPath(path) {
			return webui.Operation{}, fmt.Errorf("%w: invalid backup relative_path", webui.ErrInvalidRequest)
		}
		if _, duplicate := seen[path]; duplicate {
			return webui.Operation{}, fmt.Errorf("%w: duplicate backup relative_path", webui.ErrInvalidRequest)
		}
		seen[path] = struct{}{}
	}
	access, err := b.projectAccess(ctx, request.ProjectUID, projectMutate)
	if err != nil {
		return webui.Operation{}, err
	}
	if !access.capabilities.FSRead {
		return webui.Operation{}, fmt.Errorf("%w: filesystem read capability is unavailable", webui.ErrUnavailable)
	}
	payload, err := json.Marshal(struct {
		Version       int      `json:"version"`
		RelativePaths []string `json:"relative_paths"`
	}{Version: 1, RelativePaths: request.RelativePaths})
	if err != nil {
		return webui.Operation{}, fmt.Errorf("serverapi: encode backup operation: %w", err)
	}
	defer clear(payload)
	return b.dispatchOperation(ctx, access.agentID, request.ID, request.ProjectUID, "backup.create", "", payload)
}

func (b *Backend) RestoreBackup(ctx context.Context, request webui.BackupRestoreRequest) (webui.Operation, error) {
	if !validOperationID(request.ID) || !validOpaqueID(request.BackupID) {
		return webui.Operation{}, fmt.Errorf("%w: valid operation_id and backup_id are required", webui.ErrInvalidRequest)
	}
	access, err := b.projectAccess(ctx, request.ProjectUID, projectMutate)
	if err != nil {
		return webui.Operation{}, err
	}
	if !access.capabilities.FSWrite {
		return webui.Operation{}, fmt.Errorf("%w: filesystem write capability is unavailable", webui.ErrUnavailable)
	}
	payload := []byte(`{"version":1}`)
	defer clear(payload)
	return b.dispatchOperation(ctx, access.agentID, request.ID, request.ProjectUID, "backup.restore", request.BackupID, payload)
}

func (b *Backend) StartOperation(ctx context.Context, request webui.OperationRequest) (webui.Operation, error) {
	if !validOperationID(request.ID) || request.AgentID == "" || len(request.AgentID) > 128 || !utf8.ValidString(request.AgentID) ||
		request.Kind == "" || len(request.Kind) > 128 || !utf8.ValidString(request.Kind) ||
		len(request.Target) > 1024 || !utf8.ValidString(request.Target) {
		return webui.Operation{}, fmt.Errorf("%w: operation ID, Agent ID, and kind are required", webui.ErrInvalidRequest)
	}
	if isDedicatedMutation(request.Kind) {
		return webui.Operation{}, fmt.Errorf("%w: file and backup mutations require their dedicated typed endpoint", webui.ErrInvalidRequest)
	}
	if err := b.authorizeOperationTarget(ctx, request.AgentID, request.ProjectUID); err != nil {
		return webui.Operation{}, err
	}
	return b.dispatchOperation(ctx, request.AgentID, request.ID, request.ProjectUID, request.Kind, request.Target, nil)
}

func (b *Backend) dispatchOperation(ctx context.Context, agentID, operationID, projectUID, kind, target string, payload []byte) (webui.Operation, error) {
	if err := b.checkOperationSpec(ctx, agentID, operationID, projectUID, kind, target); err != nil {
		return webui.Operation{}, err
	}
	session, err := b.activeSession(agentID)
	if err != nil {
		return webui.Operation{}, err
	}
	payloadCopy := append([]byte(nil), payload...)
	defer clear(payloadCopy)
	response, err := session.StartOperation(ctx, producttransport.OperationRequest{
		OperationID: operationID,
		Type:        kind,
		ProjectKey:  projectUID,
		Target:      target,
		Payload:     payloadCopy,
	})
	defer clear(response.OutputTail)
	if err != nil {
		if ctx.Err() != nil {
			return webui.Operation{}, ctx.Err()
		}
		return webui.Operation{}, &liveUnavailableError{agentID: agentID, action: "operation", cause: err}
	}
	operation, err := operationFromAgent(operationID, response)
	if err != nil {
		return webui.Operation{}, err
	}
	// Once the Agent has durably accepted the operation, a browser disconnect
	// must not prevent the Server from recording the acknowledgement.
	return b.mergeAndSyncOperation(context.WithoutCancel(ctx), operationSpec{
		ID: operationID, AgentID: agentID, ProjectUID: projectUID, Kind: kind, Target: target,
	}, operation, true)
}

func operationFromAgent(operationID string, response producttransport.OperationResponse) (webui.Operation, error) {
	if response.Revision > uint64(^uint64(0)>>1) || response.Status == "" || len(response.Status) > 128 ||
		response.Phase == "" || len(response.Phase) > 128 || len(response.Error) > producttransport.DefaultMaxMessageBytes ||
		len(response.OutputTail) > maxOperationOutputBytes || !utf8.Valid(response.OutputTail) ||
		!utf8.ValidString(response.Status) || !utf8.ValidString(response.Phase) || !utf8.ValidString(response.Error) {
		return webui.Operation{}, &corruptDataError{boundary: "Agent operation response", cause: errors.New("invalid or oversized operation record")}
	}
	return webui.Operation{
		ID: operationID, Status: response.Status, Phase: response.Phase, Revision: response.Revision,
		PartialEffectsPossible: response.PartialEffectsPossible, Error: response.Error,
		OutputTail: string(response.OutputTail), OutputTruncated: response.OutputTruncated,
	}, nil
}

// GetOperation reconciles the Server cache with the Agent-authoritative
// operation record. It never treats the cache as proof that an Agent still has
// the operation.
func (b *Backend) GetOperation(ctx context.Context, agentID, operationID string) (webui.Operation, error) {
	if !validOpaqueID(agentID) || !validOperationID(operationID) {
		return webui.Operation{}, fmt.Errorf("%w: valid Agent and operation IDs are required", webui.ErrInvalidRequest)
	}
	spec, known, err := b.findOperationSpec(ctx, agentID, operationID)
	if err != nil {
		return webui.Operation{}, err
	}
	session, err := b.activeOperationControlSession(agentID)
	if err != nil {
		return webui.Operation{}, err
	}
	response, err := session.GetOperation(ctx, producttransport.GetOperationRequest{OperationID: operationID})
	if err != nil {
		if ctx.Err() != nil {
			return webui.Operation{}, ctx.Err()
		}
		return webui.Operation{}, &liveUnavailableError{agentID: agentID, action: "operation lookup", cause: err}
	}
	defer clear(response.Operation.OutputTail)
	if !response.Found {
		return webui.Operation{}, fmt.Errorf("%w: operation %q on Agent %q", webui.ErrNotFound, operationID, agentID)
	}
	if !known {
		return webui.Operation{}, fmt.Errorf("%w: Agent operation has no Server request metadata", webui.ErrConflict)
	}
	operation, err := operationFromAgent(operationID, response.Operation)
	if err != nil {
		return webui.Operation{}, err
	}
	return b.mergeAndSyncOperation(context.WithoutCancel(ctx), spec, operation, false)
}

// CancelOperation sends the only browser-authorized cancellation reason. A
// transport or browser disconnect does not synthesize this request.
func (b *Backend) CancelOperation(ctx context.Context, agentID, operationID string) (webui.OperationCancellation, error) {
	if !validOpaqueID(agentID) || !validOperationID(operationID) {
		return webui.OperationCancellation{}, fmt.Errorf("%w: valid Agent and operation IDs are required", webui.ErrInvalidRequest)
	}
	spec, known, err := b.findOperationSpec(ctx, agentID, operationID)
	if err != nil {
		return webui.OperationCancellation{}, err
	}
	session, err := b.activeOperationControlSession(agentID)
	if err != nil {
		return webui.OperationCancellation{}, err
	}
	response, err := session.CancelOperation(ctx, producttransport.CancelOperationRequest{OperationID: operationID, Reason: "USER"})
	if err != nil {
		if ctx.Err() != nil {
			return webui.OperationCancellation{}, ctx.Err()
		}
		return webui.OperationCancellation{}, &liveUnavailableError{agentID: agentID, action: "operation cancellation", cause: err}
	}
	defer clear(response.Operation.OutputTail)
	if response.Outcome == "NOT_FOUND" {
		return webui.OperationCancellation{}, fmt.Errorf("%w: operation %q on Agent %q", webui.ErrNotFound, operationID, agentID)
	}
	if !known {
		return webui.OperationCancellation{}, fmt.Errorf("%w: Agent operation has no Server request metadata", webui.ErrConflict)
	}
	operation, err := operationFromAgent(operationID, response.Operation)
	if err != nil {
		return webui.OperationCancellation{}, err
	}
	operation, err = b.mergeAndSyncOperation(context.WithoutCancel(ctx), spec, operation, false)
	if err != nil {
		return webui.OperationCancellation{}, err
	}
	return webui.OperationCancellation{Outcome: response.Outcome, Operation: operation}, nil
}

func (b *Backend) mergeAndSyncOperation(ctx context.Context, spec operationSpec, incoming webui.Operation, insert bool) (webui.Operation, error) {
	canonical, err := b.mergeOperation(ctx, spec, incoming, insert)
	if err != nil {
		return webui.Operation{}, err
	}
	if canonical.Status != "success" || !requiresTargetedProjectSync(spec.Kind) || spec.ProjectUID == "" {
		return canonical, nil
	}
	if err := b.syncProjectAfterManagedChange(ctx, spec.AgentID, spec.ProjectUID, spec.Kind == "compose.up"); err != nil {
		return webui.Operation{}, err
	}
	return canonical, nil
}

type operationSpec struct {
	ID, AgentID, ProjectUID, Kind, Target string
}

type operationSummary struct {
	Version                int    `json:"version"`
	Target                 string `json:"target,omitempty"`
	PartialEffectsPossible bool   `json:"partial_effects_possible"`
	Error                  string `json:"error,omitempty"`
}

func (b *Backend) checkOperationSpec(ctx context.Context, agentID, operationID, projectUID, kind, target string) error {
	var storedAgent, storedKind, storedProject string
	var rawSummary string
	err := b.store.DB().QueryRowContext(ctx, `
		SELECT agent_id, COALESCE(project_uid, ''), kind, summary_json
		FROM operations WHERE id = ?
	`, operationID).Scan(&storedAgent, &storedProject, &storedKind, &rawSummary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serverapi: load operation specification: %w", err)
	}
	var summary operationSummary
	if err := decodeStrictJSON([]byte(rawSummary), &summary); err != nil || summary.Version != 1 {
		return &corruptDataError{boundary: "operations.summary_json", cause: errors.New("invalid operation summary")}
	}
	if storedAgent != agentID || storedProject != projectUID || storedKind != kind || summary.Target != target {
		return fmt.Errorf("%w: operation ID is already bound to a different Agent or request", webui.ErrConflict)
	}
	return nil
}

func (b *Backend) findOperationSpec(ctx context.Context, agentID, operationID string) (operationSpec, bool, error) {
	var spec operationSpec
	var project sql.NullString
	var rawSummary string
	err := b.store.DB().QueryRowContext(ctx, `
		SELECT id, agent_id, project_uid, kind, summary_json FROM operations WHERE id = ?
	`, operationID).Scan(&spec.ID, &spec.AgentID, &project, &spec.Kind, &rawSummary)
	if errors.Is(err, sql.ErrNoRows) {
		return operationSpec{}, false, nil
	}
	if err != nil {
		return operationSpec{}, false, fmt.Errorf("serverapi: load operation: %w", err)
	}
	if spec.AgentID != agentID {
		return operationSpec{}, false, fmt.Errorf("%w: operation ID belongs to a different Agent", webui.ErrConflict)
	}
	spec.ProjectUID = project.String
	var summary operationSummary
	if err := decodeStrictJSON([]byte(rawSummary), &summary); err != nil || summary.Version != 1 {
		return operationSpec{}, false, &corruptDataError{boundary: "operations.summary_json", cause: errors.New("invalid operation summary")}
	}
	spec.Target = summary.Target
	return spec, true, nil
}

func (b *Backend) mergeOperation(ctx context.Context, spec operationSpec, incoming webui.Operation, insert bool) (webui.Operation, error) {
	if err := b.lockOperationMerge(ctx); err != nil {
		return webui.Operation{}, err
	}
	defer b.unlockOperationMerge()
	summaryBytes, err := json.Marshal(operationSummary{
		Version: 1, Target: spec.Target, PartialEffectsPossible: incoming.PartialEffectsPossible, Error: incoming.Error,
	})
	if err != nil {
		return webui.Operation{}, fmt.Errorf("serverapi: encode operation summary: %w", err)
	}
	defer clear(summaryBytes)
	project := any(nil)
	if spec.ProjectUID != "" {
		project = spec.ProjectUID
	}
	if insert {
		_, err = b.store.DB().ExecContext(ctx, `
			INSERT INTO operations(
				id, agent_id, project_uid, kind, status, phase, revision, actor,
				requested_at, summary_json, output_tail, output_truncated
			) VALUES(?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				status = excluded.status, phase = excluded.phase, revision = excluded.revision,
				summary_json = excluded.summary_json, output_tail = excluded.output_tail,
				output_truncated = excluded.output_truncated
			WHERE operations.agent_id = excluded.agent_id
			  AND COALESCE(operations.project_uid, '') = COALESCE(excluded.project_uid, '')
			  AND operations.kind = excluded.kind
			  AND COALESCE(json_extract(operations.summary_json, '$.target'), '') = ?
			  AND operations.revision < excluded.revision
		`, spec.ID, spec.AgentID, project, spec.Kind, incoming.Status, incoming.Phase, incoming.Revision,
			time.Now().UTC().Format(time.RFC3339Nano), string(summaryBytes), []byte(incoming.OutputTail), incoming.OutputTruncated,
			spec.Target)
	} else {
		_, err = b.store.DB().ExecContext(ctx, `
			UPDATE operations SET
				status = ?, phase = ?, revision = ?, summary_json = ?, output_tail = ?, output_truncated = ?
			WHERE id = ? AND agent_id = ?
			  AND COALESCE(project_uid, '') = ? AND kind = ?
			  AND COALESCE(json_extract(summary_json, '$.target'), '') = ?
			  AND revision < ?
		`, incoming.Status, incoming.Phase, incoming.Revision, string(summaryBytes), []byte(incoming.OutputTail),
			incoming.OutputTruncated, spec.ID, spec.AgentID, spec.ProjectUID, spec.Kind, spec.Target, incoming.Revision)
	}
	if err != nil {
		return webui.Operation{}, fmt.Errorf("serverapi: persist operation record: %w", err)
	}
	canonical, storedSpec, err := b.loadStoredOperation(ctx, spec.ID)
	if err != nil {
		return webui.Operation{}, err
	}
	if storedSpec.AgentID != spec.AgentID || storedSpec.ProjectUID != spec.ProjectUID || storedSpec.Kind != spec.Kind || storedSpec.Target != spec.Target {
		return webui.Operation{}, fmt.Errorf("%w: operation ID is already bound to a different Agent or request", webui.ErrConflict)
	}
	if canonical.Revision == incoming.Revision && canonical != incoming {
		return webui.Operation{}, fmt.Errorf("%w: Agent changed an operation without increasing its revision", webui.ErrConflict)
	}
	return canonical, nil
}

func (b *Backend) lockOperationMerge(ctx context.Context) error {
	select {
	case b.operationMergeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) unlockOperationMerge() { <-b.operationMergeGate }

func (b *Backend) loadStoredOperation(ctx context.Context, operationID string) (webui.Operation, operationSpec, error) {
	var operation webui.Operation
	var spec operationSpec
	var project sql.NullString
	var revision int64
	var rawSummary string
	err := b.store.DB().QueryRowContext(ctx, `
		SELECT id, agent_id, project_uid, kind, status, phase, revision, summary_json,
		       COALESCE(output_tail, X''), output_truncated
		FROM operations WHERE id = ?
	`, operationID).Scan(&operation.ID, &spec.AgentID, &project, &spec.Kind, &operation.Status, &operation.Phase,
		&revision, &rawSummary, &operation.OutputTail, &operation.OutputTruncated)
	if err != nil {
		return webui.Operation{}, operationSpec{}, fmt.Errorf("serverapi: load canonical operation: %w", err)
	}
	if revision < 0 || !utf8.ValidString(operation.OutputTail) {
		return webui.Operation{}, operationSpec{}, &corruptDataError{boundary: "operations", cause: errors.New("invalid operation row")}
	}
	operation.Revision = uint64(revision)
	spec.ID, spec.ProjectUID = operation.ID, project.String
	var summary operationSummary
	if err := decodeStrictJSON([]byte(rawSummary), &summary); err != nil || summary.Version != 1 {
		return webui.Operation{}, operationSpec{}, &corruptDataError{boundary: "operations.summary_json", cause: errors.New("invalid operation summary")}
	}
	spec.Target = summary.Target
	operation.PartialEffectsPossible, operation.Error = summary.PartialEffectsPossible, summary.Error
	return operation, spec, nil
}

func (b *Backend) activeOperationControlSession(agentID string) (producttransport.OperationControlSession, error) {
	session, err := b.activeSession(agentID)
	if err != nil {
		return nil, err
	}
	control, ok := session.(producttransport.OperationControlSession)
	if !ok {
		return nil, &liveUnavailableError{agentID: agentID, action: "operation control", cause: producttransport.ErrHandlerUnavailable}
	}
	return control, nil
}

func (b *Backend) OpenLogs(ctx context.Context, request webui.LiveRequest) (webui.LogStream, error) {
	if err := b.authorizeLiveRequest(ctx, request); err != nil {
		return nil, err
	}
	session, err := b.activeSession(request.AgentID)
	if err != nil {
		return nil, err
	}
	stream, err := session.OpenLogs(ctx, producttransport.LogRequest{
		ContainerID: request.ContainerID, Follow: request.Follow, TailLines: request.TailLines,
		ShowStdout: request.ShowStdout, ShowStderr: request.ShowStderr, Timestamps: request.Timestamps,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: request.AgentID, action: "logs", cause: err}
	}
	return &liveLogStream{stream: stream}, nil
}

func (b *Backend) OpenStats(ctx context.Context, request webui.LiveRequest) (webui.StatsStream, error) {
	if err := b.authorizeLiveRequest(ctx, request); err != nil {
		return nil, err
	}
	session, err := b.activeSession(request.AgentID)
	if err != nil {
		return nil, err
	}
	stream, err := session.OpenStats(ctx, producttransport.StatsRequest{ContainerID: request.ContainerID})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: request.AgentID, action: "stats", cause: err}
	}
	key := liveStatsKey{agentID: request.AgentID, containerID: request.ContainerID}
	b.liveMu.Lock()
	state := b.liveStats[key]
	state.viewers++
	b.liveStats[key] = state
	b.liveMu.Unlock()
	return &liveStatsStream{backend: b, key: key, stream: stream}, nil
}

func (b *Backend) authorizeLiveRequest(ctx context.Context, request webui.LiveRequest) error {
	if !canonicalContainerID.MatchString(request.ContainerID) {
		return fmt.Errorf("%w: valid Agent ID and canonical container ID are required", webui.ErrInvalidRequest)
	}
	return b.authorizeAgent(ctx, request.AgentID)
}

// authorizeAgent admits a live request only for an Agent the Server still has a
// row for. A host-scoped stream carries no container ID to check, so this is
// the whole check for it.
func (b *Backend) authorizeAgent(ctx context.Context, agentID string) error {
	if agentID == "" || !utf8.ValidString(agentID) {
		return fmt.Errorf("%w: valid Agent ID is required", webui.ErrInvalidRequest)
	}
	var exists int
	if err := b.store.DB().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM agents WHERE id = ? AND retired_at IS NULL)`, agentID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("serverapi: authorize live request: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("%w: Agent is not in the Server cache", webui.ErrNotFound)
	}
	return nil
}

type liveLogStream struct {
	stream producttransport.LogReceiveStream
	once   sync.Once
}

func (s *liveLogStream) Recv(ctx context.Context) (webui.LogEvent, error) {
	event, err := s.stream.Recv(ctx)
	if err != nil {
		_ = s.Close()
		return webui.LogEvent{}, err
	}
	clientError := ""
	if event.Error != "" {
		clientError = "log stream ended"
	}
	return webui.LogEvent{
		Data: append([]byte(nil), event.Data...), Stream: event.Stream, LineCount: event.LineCount,
		Timestamp: event.Timestamp, DroppedBytes: event.DroppedBytes, DroppedLines: event.DroppedLines,
		Terminal: event.Terminal, Error: clientError,
	}, nil
}

func (s *liveLogStream) Close() error {
	var err error
	s.once.Do(func() { err = s.stream.Close() })
	return err
}

type liveStatsStream struct {
	backend *Backend
	key     liveStatsKey
	stream  producttransport.StatsReceiveStream
	once    sync.Once
}

func (s *liveStatsStream) Recv(ctx context.Context) (webui.StatsSample, error) {
	sample, err := s.stream.Recv(ctx)
	if err != nil {
		_ = s.Close()
		return webui.StatsSample{}, err
	}
	value := webui.StatsSample{
		ContainerID: sample.ContainerID, ObservedAt: sample.ObservedAt, CPUPercent: sample.CPUPercent,
		MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit, NetworkRX: sample.NetworkRX,
		NetworkTX: sample.NetworkTX, BlockRead: sample.BlockRead, BlockWrite: sample.BlockWrite,
		RestartCount: sample.RestartCount, Health: sample.Health, Uptime: sample.Uptime,
	}
	s.backend.liveMu.Lock()
	state, active := s.backend.liveStats[s.key]
	if active {
		state.latest, state.has = value, true
		s.backend.liveStats[s.key] = state
	}
	s.backend.liveMu.Unlock()
	return value, nil
}

func (s *liveStatsStream) Close() error {
	var err error
	s.once.Do(func() {
		err = s.stream.Close()
		s.backend.liveMu.Lock()
		state := s.backend.liveStats[s.key]
		state.viewers--
		if state.viewers <= 0 {
			delete(s.backend.liveStats, s.key)
		} else {
			s.backend.liveStats[s.key] = state
		}
		s.backend.liveMu.Unlock()
	})
	return err
}

func (b *Backend) currentStats(agentID, containerID string) (webui.StatsSample, bool) {
	b.liveMu.RLock()
	defer b.liveMu.RUnlock()
	state, ok := b.liveStats[liveStatsKey{agentID: agentID, containerID: containerID}]
	return state.latest, ok && state.has
}

func (b *Backend) loadAgents(ctx context.Context) ([]agentRow, error) {
	rows, err := b.store.DB().QueryContext(ctx, `
		SELECT id, display_name, capabilities_json, projects_scanned_at, project_scan_status_json
		FROM agents WHERE retired_at IS NULL ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("serverapi: query agents: %w", err)
	}
	defer rows.Close()
	var result []agentRow
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("serverapi: iterate agents: %w", err)
	}
	return result, nil
}

func (b *Backend) loadAgent(ctx context.Context, agentID string) (agentRow, error) {
	row := b.store.DB().QueryRowContext(ctx, `
		SELECT id, display_name, capabilities_json, projects_scanned_at, project_scan_status_json
		FROM agents WHERE id = ? AND retired_at IS NULL
	`, agentID)
	agent, err := scanAgent(row)
	if err != nil {
		return agentRow{}, err
	}
	return agent, nil
}

type scanner interface{ Scan(...any) error }

func scanAgent(row scanner) (agentRow, error) {
	var agent agentRow
	var rawCapabilities, rawProjectScan string
	var scannedAt sql.NullString
	if err := row.Scan(&agent.id, &agent.displayName, &rawCapabilities, &scannedAt, &rawProjectScan); err != nil {
		return agentRow{}, err
	}
	if agent.id == "" || agent.displayName == "" || !utf8.ValidString(agent.id) || !utf8.ValidString(agent.displayName) {
		return agentRow{}, &corruptDataError{boundary: "agents row", cause: errors.New("empty or invalid UTF-8 identity")}
	}
	if err := decodeStrictJSON([]byte(rawCapabilities), &agent.capabilities); err != nil {
		return agentRow{}, &corruptDataError{boundary: "agents.capabilities_json", cause: err}
	}
	if scannedAt.Valid {
		watermark, err := time.Parse(projectScanTimeFormat, scannedAt.String)
		if err != nil || watermark.IsZero() {
			return agentRow{}, &corruptDataError{boundary: "agents.projects_scanned_at", cause: errors.New("invalid project scan watermark")}
		}
		var status agentProjectScanStatus
		if err := decodeStrictJSON([]byte(rawProjectScan), &status); err != nil || !status.ScannedAt.Equal(watermark) {
			return agentRow{}, &corruptDataError{boundary: "agents.project_scan_status_json", cause: errors.New("invalid or mismatched project scan status")}
		}
		agent.projectScan = &webui.ProjectScan{
			ScannedAt: status.ScannedAt, Truncated: status.Truncated, StopReason: status.StopReason,
			DirectoriesSeen: status.DirectoriesSeen, LastScannedPath: status.LastScannedPath,
		}
	} else if strings.TrimSpace(rawProjectScan) != "{}" {
		return agentRow{}, &corruptDataError{boundary: "agents.project_scan_status_json", cause: errors.New("project scan status exists without a watermark")}
	}
	return agent, nil
}

func (b *Backend) loadProjects(ctx context.Context) ([]webui.Project, error) {
	rows, err := b.store.DB().QueryContext(ctx, `
		SELECT projects.project_uid, projects.agent_id, projects.working_dir, projects.name,
		       projects.applied_fingerprints_json, projects.flags_json
		FROM projects JOIN agents ON agents.id = projects.agent_id
		WHERE agents.retired_at IS NULL
		ORDER BY projects.project_uid
	`)
	if err != nil {
		return nil, fmt.Errorf("serverapi: query projects: %w", err)
	}
	defer rows.Close()
	var projects []webui.Project
	for rows.Next() {
		var project webui.Project
		var rawApplied, rawFlags string
		if err := rows.Scan(&project.UID, &project.AgentID, &project.WorkingDir, &project.Name, &rawApplied, &rawFlags); err != nil {
			return nil, fmt.Errorf("serverapi: scan project: %w", err)
		}
		if project.UID == "" || project.AgentID == "" || project.WorkingDir == "" ||
			!utf8.ValidString(project.UID) || !utf8.ValidString(project.AgentID) ||
			!utf8.ValidString(project.WorkingDir) || !utf8.ValidString(project.Name) {
			return nil, &corruptDataError{boundary: "projects row", cause: errors.New("empty or invalid UTF-8 identity")}
		}
		var flags projectFlags
		if err := decodeStrictJSON([]byte(rawFlags), &flags); err != nil {
			return nil, &corruptDataError{boundary: "projects.flags_json", cause: err}
		}
		applied, err := appliedFingerprint(rawApplied)
		if err != nil {
			return nil, &corruptDataError{boundary: "projects.applied_fingerprints_json", cause: err}
		}
		if flags.LastVerifiedAt != "" {
			verifiedAt, err := time.Parse(time.RFC3339Nano, flags.LastVerifiedAt)
			if err != nil || verifiedAt.IsZero() {
				return nil, &corruptDataError{boundary: "projects.flags_json", cause: errors.New("invalid last verified timestamp")}
			}
			project.LastVerifiedAt = &verifiedAt
		}
		project.Collision = flags.Collision
		project.Managed = managedProject(flags)
		project.UnmanagedReason = flags.UnmanagedReason
		project.ContainerIDs = append([]string(nil), flags.ContainerIDs...)
		project.Services = append([]string(nil), flags.Services...)
		project.IncludedBy = append([]string(nil), flags.IncludedBy...)
		project.SourceReferences = make([]webui.SourceReference, len(flags.SourceReferences))
		for index, reference := range flags.SourceReferences {
			project.SourceReferences[index] = webui.SourceReference{
				Kind: reference.Kind, Path: reference.Path, Accessible: reference.Accessible, ReadOnly: reference.ReadOnly,
			}
		}
		project.SourceGraphComplete = flags.SourceGraphComplete
		project.ReadOnly = flags.ReadOnly || flags.Collision
		project.RestoreRecoveryRequired = flags.RestoreRecoveryRequired
		project.Present = !flags.Missing
		project.Stale = flags.Stale
		project.ComposeExecutable = flags.ComposeExecutable
		project.FilesystemWritable = flags.FilesystemWritable
		project.CapabilityReason = flags.CapabilityReason
		project.CurrentFingerprint = flags.CurrentFingerprint
		project.AppliedFingerprint = applied
		project.LastVerifiedFingerprint = flags.LastVerifiedFingerprint
		project.Drift = flags.Drift
		if project.Drift == "" {
			project.Drift = string(projectmodel.DriftStatus(applied, flags.CurrentFingerprint))
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("serverapi: iterate projects: %w", err)
	}
	return projects, nil
}

type projectAccessState struct {
	agentID      string
	capabilities storedCapabilities
	flags        projectFlags
}

// projectIntent is what the caller is about to do with the project. It is a
// required argument rather than a follow-up check because the follow-up check
// is the thing that gets forgotten: the read-only guard was written out at
// three endpoints by hand, and the fourth - backup creation - dispatched a
// durable operation the Agent then refused. Every endpoint already passes
// through projectAccess, so this is the one place that cannot be skipped.
type projectIntent int

const (
	projectRead projectIntent = iota
	projectMutate
)

func (b *Backend) projectAccess(ctx context.Context, projectUID string, intent projectIntent) (projectAccessState, error) {
	if !validOpaqueID(projectUID) {
		return projectAccessState{}, fmt.Errorf("%w: project UID is required", webui.ErrInvalidRequest)
	}
	var state projectAccessState
	var rawCapabilities, rawFlags string
	err := b.store.DB().QueryRowContext(ctx, `
		SELECT agents.id, agents.capabilities_json, projects.flags_json
		FROM projects JOIN agents ON agents.id = projects.agent_id
		WHERE projects.project_uid = ? AND agents.retired_at IS NULL
	`, projectUID).Scan(&state.agentID, &rawCapabilities, &rawFlags)
	if errors.Is(err, sql.ErrNoRows) {
		return projectAccessState{}, fmt.Errorf("%w: project %q", webui.ErrNotFound, projectUID)
	}
	if err != nil {
		return projectAccessState{}, fmt.Errorf("serverapi: load project access: %w", err)
	}
	if state.agentID == "" || !utf8.ValidString(state.agentID) {
		return projectAccessState{}, &corruptDataError{boundary: "project access", cause: errors.New("invalid Agent ID")}
	}
	if err := decodeStrictJSON([]byte(rawCapabilities), &state.capabilities); err != nil {
		return projectAccessState{}, &corruptDataError{boundary: "agents.capabilities_json", cause: err}
	}
	if err := decodeStrictJSON([]byte(rawFlags), &state.flags); err != nil {
		return projectAccessState{}, &corruptDataError{boundary: "projects.flags_json", cause: err}
	}
	state.applyLiveFilesystemCapability(ctx, b)
	// The one place a mutating endpoint cannot skip.
	if intent == projectMutate && (state.flags.ReadOnly || state.flags.Collision) {
		return projectAccessState{}, fmt.Errorf("%w: project is read-only", webui.ErrConflict)
	}
	return state, nil
}

// applyLiveFilesystemCapability prefers the connected Agent's reported
// filesystem capability over the stored row, exactly as Docker and Compose
// capability are read live. The Agent owns the per-root identical-path
// self-check, so a live report is always the newer truth; an Agent one protocol
// version behind reports nothing and the stored value is left untouched.
func (state *projectAccessState) applyLiveFilesystemCapability(ctx context.Context, b *Backend) {
	session, err := b.activeSession(state.agentID)
	if err != nil {
		return
	}
	// One unreachable Agent must not set the latency of the fleet view. A
	// partitioned Agent does not refuse a heartbeat - its packets are dropped -
	// so without a bound of its own this call waits for the transport to give
	// up, and every other host's row waits behind it. The reconcile pass above
	// already bounds its per-Agent work the same way.
	probeCtx, cancelProbe := context.WithTimeout(ctx, hostProbeTimeout)
	heartbeat, err := session.Heartbeat(probeCtx)
	cancelProbe()
	if err != nil {
		return
	}
	capability := heartbeat.Capability
	if !capability.FSRead && !capability.FSWrite && capability.FSReadReason == "" && capability.FSWriteReason == "" {
		return
	}
	state.capabilities.FSRead = capability.FSRead
	state.capabilities.FSWrite = capability.FSWrite
	state.capabilities.FSReadReason = capability.FSReadReason
	state.capabilities.FSWriteReason = capability.FSWriteReason
}

func validOperationID(value string) bool { return validOpaqueID(value) }

func validOpaqueID(value string) bool {
	return safeOpaqueID.MatchString(value) && value != "." && value != ".."
}

func validManagedPath(path string) bool {
	if path == "" || len(path) > 1024 || !utf8.ValidString(path) || strings.Contains(path, "/") || strings.ContainsRune(path, 0) {
		return false
	}
	if path == ".env" || strings.HasPrefix(path, ".env.") && len(path) > len(".env.") {
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

func fileWriteKind(path string) (string, bool) {
	if path == ".env" || strings.HasPrefix(path, ".env.") && len(path) > len(".env.") {
		return "env.write", true
	}
	if (strings.HasPrefix(path, "compose.override.") || strings.HasPrefix(path, "docker-compose.override.")) &&
		(strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
		return "override.write", true
	}
	switch path {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
		return "compose.file.write", true
	}
	if strings.HasPrefix(path, "compose.") && strings.HasSuffix(path, ".yaml") && !strings.HasPrefix(path, "compose.override.") {
		return "compose.file.write", true
	}
	return "", false
}

func isEnvironmentPath(path string) bool {
	return path == ".env" || strings.HasPrefix(path, ".env.")
}

func validLineEndings(value string) bool {
	return value == "none" || value == "lf" || value == "crlf" || value == "mixed"
}

func validBackupTrigger(value string) bool {
	return value == "manual" || value == "pre_write" || value == "pre_restore"
}

func isDedicatedMutation(kind string) bool {
	switch kind {
	case "compose.file.write", "env.write", "override.write", "backup.create", "backup.restore":
		return true
	default:
		return false
	}
}

func requiresTargetedProjectSync(kind string) bool {
	switch kind {
	case "compose.up", "compose.file.write", "env.write", "override.write", "backup.restore":
		return true
	default:
		return false
	}
}

func (b *Backend) authorizeOperationTarget(ctx context.Context, agentID, projectUID string) error {
	var exists int
	if projectUID == "" {
		if err := b.store.DB().QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM agents WHERE id = ? AND retired_at IS NULL)`, agentID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("serverapi: authorize Agent: %w", err)
		}
	} else {
		// projectRead, then the guard by hand, and the order is the point: a
		// project belonging to a different Agent has to answer NOT_FOUND, not
		// CONFLICT. Asking projectAccess to refuse a mutation here would report
		// "read-only" for somebody else's project and confirm it exists.
		access, err := b.projectAccess(ctx, projectUID, projectRead)
		if errors.Is(err, webui.ErrNotFound) {
			return fmt.Errorf("%w: operation target is not in the Server cache", webui.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if access.agentID != agentID {
			return fmt.Errorf("%w: operation target is not in the Server cache", webui.ErrNotFound)
		}
		if access.flags.ReadOnly || access.flags.Collision {
			return fmt.Errorf("%w: project is read-only", webui.ErrConflict)
		}
		exists = 1
	}
	if exists == 0 {
		return fmt.Errorf("%w: operation target is not in the Server cache", webui.ErrNotFound)
	}
	return nil
}

func (b *Backend) activeSession(agentID string) (producttransport.ControlSession, error) {
	session, ok := b.registry.Current(agentID)
	if !ok || session.State() != producttransport.StateActive {
		return nil, &OfflineError{AgentID: agentID}
	}
	return session, nil
}

func (b *Backend) liveHost(ctx context.Context, agent agentRow) webui.Host {
	host := webui.Host{ID: agent.id, DisplayName: agent.displayName, State: string(producttransport.StateOffline), ProjectScan: agent.projectScan}
	session, ok := b.registry.Current(agent.id)
	if !ok {
		host.Capabilities = disabledCapabilities("agent offline")
		return host
	}
	state := session.State()
	host.State = string(state)
	if state != producttransport.StateActive {
		host.Capabilities = disabledCapabilities("agent session is not active")
		return host
	}
	// Bounded for the same reason as the refresh path above: the dashboard
	// waits for every host row, so an unreachable Agent would otherwise set
	// the latency of the whole fleet view.
	probeCtx, cancelProbe := context.WithTimeout(ctx, hostProbeTimeout)
	heartbeat, err := session.Heartbeat(probeCtx)
	cancelProbe()
	if err != nil {
		host.State = string(session.State())
		host.Capabilities = disabledCapabilities("heartbeat unavailable")
		return host
	}
	capability := heartbeat.Capability
	host.Capabilities.Connection = webCapability(capability.ConnectionReady, capability.Reason, "connection not ready")
	if !capability.ConnectionReady {
		host.Capabilities.Docker = webCapability(false, capability.Reason, "connection not ready")
		host.Capabilities.Compose = webCapability(false, capability.Reason, "connection not ready")
		host.Capabilities.FSRead = webCapability(false, capability.Reason, "connection not ready")
		host.Capabilities.FSWrite = webCapability(false, capability.Reason, "connection not ready")
		host.Capabilities.Metrics = webCapability(false, capability.Reason, "connection not ready")
		return host
	}
	host.Capabilities.Docker = webCapability(capability.DockerReady, capability.Reason, "Docker unavailable")
	host.Capabilities.Compose = webCapability(capability.ComposeReady, capability.Reason, "Compose unavailable")
	// An Agent built before live metrics leaves the flag false and says nothing
	// about it, which is exactly why the reason is written here rather than
	// taken from the Agent: silence from an older build is the answer, and this
	// is the sentence that states it.
	host.Capabilities.Metrics = webCapability(capability.MetricsMatrix, "", "live metrics are not available on this Agent")
	fsRead, fsReadReason := agent.capabilities.FSRead, agent.capabilities.FSReadReason
	fsWrite, fsWriteReason := agent.capabilities.FSWrite, agent.capabilities.FSWriteReason
	if capability.FSRead || capability.FSWrite || capability.FSReadReason != "" || capability.FSWriteReason != "" {
		fsRead, fsReadReason = capability.FSRead, capability.FSReadReason
		fsWrite, fsWriteReason = capability.FSWrite, capability.FSWriteReason
	}
	host.Capabilities.FSRead = webCapability(fsRead, fsReadReason, "filesystem read capability not reported")
	host.Capabilities.FSWrite = webCapability(fsWrite, fsWriteReason, "filesystem write capability not reported")
	return host
}

func disabledCapabilities(reason string) webui.Capabilities {
	value := webui.Capability{Reason: reason}
	return webui.Capabilities{Connection: value, Docker: value, Compose: value, Discovery: value, Metrics: value, OperationRecovery: value, FSRead: value, FSWrite: value}
}

func webCapability(enabled bool, reason, fallback string) webui.Capability {
	if enabled {
		return webui.Capability{Enabled: true, Reason: reason}
	}
	if reason == "" {
		reason = fallback
	}
	return webui.Capability{Reason: reason}
}

func decodeStrictJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not UTF-8")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("JSON null is not allowed")
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("invalid JSON object closing delimiter")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array closing delimiter")
		}
		return nil
	default:
		return errors.New("invalid JSON opening delimiter")
	}
}
