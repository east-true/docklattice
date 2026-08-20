// Package webui provides the Server's HTTP API and embedded browser UI.
//
// The package deliberately owns no durable Docker, log, metric, or file-content
// state. A Backend produces current snapshots and dispatches operations to the
// Agent-facing product layer.
package webui

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound       = errors.New("resource not found")
	ErrConflict       = errors.New("operation conflicts with current state")
	ErrUnavailable    = errors.New("capability unavailable")
	ErrBusy           = errors.New("server is busy")
	ErrInvalidRequest = errors.New("invalid request")
	ErrTooLarge       = errors.New("request or response exceeds a safe transport limit")
)

type Capability struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

type Capabilities struct {
	Connection        Capability `json:"connection"`
	Docker            Capability `json:"docker"`
	Compose           Capability `json:"compose"`
	Discovery         Capability `json:"discovery"`
	OperationRecovery Capability `json:"operation_recovery"`
	FSRead            Capability `json:"fs_read"`
	FSWrite           Capability `json:"fs_write"`
}

type Host struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	State        string       `json:"state"`
	Capabilities Capabilities `json:"capabilities"`
	ProjectScan  *ProjectScan `json:"project_scan,omitempty"`
}

// HostContainer, HostImage, HostNetwork, and HostVolume are deliberately curated live views.
// Docker labels, network options, volume options, and volume mountpoints are
// not part of the Server API and therefore cannot be reflected to browsers.
type HostContainer struct {
	ID     string   `json:"id"`
	Names  []string `json:"names"`
	Image  string   `json:"image"`
	State  string   `json:"state"`
	Status string   `json:"status"`
}

type HostImage struct {
	ID          string   `json:"id"`
	RepoTags    []string `json:"repo_tags"`
	RepoDigests []string `json:"repo_digests"`
	Created     int64    `json:"created_unix"`
	Size        int64    `json:"size_bytes"`
	Containers  int64    `json:"containers"`
}

type HostNetwork struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Scope      string `json:"scope"`
	Internal   bool   `json:"internal"`
	Attachable bool   `json:"attachable"`
	Ingress    bool   `json:"ingress"`
}

type HostVolume struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at,omitempty"`
}

type ProjectScan struct {
	ScannedAt       time.Time `json:"scanned_at"`
	Truncated       bool      `json:"truncated"`
	StopReason      string    `json:"stop_reason,omitempty"`
	DirectoriesSeen int       `json:"directories_seen"`
	LastScannedPath string    `json:"last_scanned_path,omitempty"`
}

type Project struct {
	UID                 string            `json:"uid"`
	AgentID             string            `json:"agent_id"`
	WorkingDir          string            `json:"working_dir"`
	Name                string            `json:"name"`
	Managed             bool              `json:"managed"`
	UnmanagedReason     string            `json:"unmanaged_reason,omitempty"`
	ContainerIDs        []string          `json:"container_ids,omitempty"`
	Services            []string          `json:"services,omitempty"`
	IncludedBy          []string          `json:"included_by,omitempty"`
	SourceReferences    []SourceReference `json:"source_references,omitempty"`
	SourceGraphComplete bool              `json:"source_graph_complete"`
	Present             bool              `json:"present"`
	Stale               bool              `json:"stale"`
	ReadOnly            bool              `json:"read_only"`
	Collision           bool              `json:"collision"`
	ComposeExecutable   bool              `json:"compose_executable"`
	FilesystemWritable  bool              `json:"filesystem_writable"`
	// RestoreRecoveryRequired means a restore failed and its rollback failed
	// too. The project files are in an unknown state and every mutation is
	// refused until an operator resolves it. read_only is true as well; this
	// field says why, because "not writable" and "damaged" are different
	// things for anyone deciding what to do next.
	RestoreRecoveryRequired bool       `json:"restore_recovery_required"`
	CapabilityReason        string     `json:"capability_reason,omitempty"`
	CurrentFingerprint      string     `json:"current_fingerprint,omitempty"`
	AppliedFingerprint      string     `json:"applied_fingerprint,omitempty"`
	LastVerifiedFingerprint string     `json:"last_verified_fingerprint,omitempty"`
	LastVerifiedAt          *time.Time `json:"last_verified_at,omitempty"`
	Drift                   string     `json:"drift"`
}

// SourceReference is content-free Compose include/extends provenance. It
// never grants browser access to a file; only the Agent safefile API can read
// an approved relative path.
type SourceReference struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	ReadOnly   bool   `json:"read_only"`
}

type Dashboard struct {
	Hosts    []Host    `json:"hosts"`
	Projects []Project `json:"projects"`
}

const (
	DefaultAuditPageSize = 100
	MaxAuditPageSize     = 500
)

type AuditCursor struct {
	Incarnation uint64 `json:"incarnation"`
	Seq         uint64 `json:"seq"`
}

type AuditPageRequest struct {
	Cursor *AuditCursor `json:"-"`
	Limit  int          `json:"-"`
}

// AuditEvent is a curated view of one canonical event. The stored metadata
// payload and its arbitrary attributes deliberately have no browser field.
type AuditEvent struct {
	Cursor              AuditCursor `json:"cursor"`
	OccurredAt          time.Time   `json:"occurred_at"`
	LastAt              time.Time   `json:"last_at"`
	Kind                string      `json:"kind"`
	ResourceType        string      `json:"resource_type"`
	ResourceID          string      `json:"resource_id,omitempty"`
	Action              string      `json:"action"`
	Actor               string      `json:"actor,omitempty"`
	ProjectUID          string      `json:"project_uid,omitempty"`
	OperationID         string      `json:"operation_id,omitempty"`
	Count               uint64      `json:"count"`
	PreviousIncarnation uint64      `json:"previous_incarnation,omitempty"`
	KnownDurableThrough *uint64     `json:"known_durable_through,omitempty"`
	ContinuityReason    string      `json:"continuity_reason,omitempty"`
}

type AuditCoverageStart struct {
	Cursor AuditCursor `json:"cursor"`
	Reason string      `json:"reason"`
}

type AuditCoverageGap struct {
	Type          string      `json:"type"`
	From          AuditCursor `json:"from"`
	Until         AuditCursor `json:"until"`
	Precision     string      `json:"precision"`
	Source        string      `json:"source"`
	Reason        string      `json:"reason,omitempty"`
	EstablishedAt time.Time   `json:"established_at"`
}

type AuditCoverage struct {
	Established                 bool                `json:"established"`
	Start                       *AuditCoverageStart `json:"start,omitempty"`
	DeliveryNext                *AuditCursor        `json:"delivery_next,omitempty"`
	ACK                         *AuditCursor        `json:"ack,omitempty"`
	CoverageRevisionSeen        uint64              `json:"coverage_revision_seen"`
	CoverageRevisionCurrent     uint64              `json:"coverage_revision_current"`
	ACKWatermarkStalledSeconds  int64               `json:"ack_watermark_stalled_seconds"`
	ACKBlockedWhileIngesting    bool                `json:"ack_blocked_while_ingesting"`
	ACKBlockedWhileIngestingFor int64               `json:"ack_blocked_while_ingesting_seconds"`
	IngestedUnackedRecords      int64               `json:"ingested_unacked_records"`
	EffectiveGapRecords         int64               `json:"effective_gap_records"`
	AgentGapClaimsTotal         int64               `json:"agent_gap_claims_total"`
	Gaps                        []AuditCoverageGap  `json:"gaps"`
	UnknownIncarnations         []uint64            `json:"unknown_incarnations"`
	CoverageEntriesTruncated    bool                `json:"coverage_entries_truncated"`
}

type AuditPage struct {
	AgentID    string        `json:"agent_id"`
	ProjectUID string        `json:"project_uid,omitempty"`
	Events     []AuditEvent  `json:"events"`
	NextCursor *AuditCursor  `json:"next_cursor,omitempty"`
	Coverage   AuditCoverage `json:"coverage"`
}

type OperationRequest struct {
	ID         string `json:"operation_id"`
	AgentID    string `json:"agent_id"`
	ProjectUID string `json:"project_uid,omitempty"`
	Kind       string `json:"kind"`
	Target     string `json:"target,omitempty"`
}

type ProjectFile struct {
	RelativePath string    `json:"relative_path"`
	Content      string    `json:"content"`
	SHA256       string    `json:"sha256"`
	MTime        time.Time `json:"mtime"`
	Mode         uint32    `json:"mode"`
	LineEndings  string    `json:"line_endings"`
	Secret       bool      `json:"secret"`
}

// ComposeQuery is a closed set of project-scoped read options. The browser
// cannot pass filesystem paths or arbitrary Compose CLI flags.
type ComposeQuery struct {
	Services []string `json:"services"`
	All      bool     `json:"all"`
}

// ComposeOutput is a transient bounded CLI view. It is not persisted by the
// Server and therefore cannot become a configuration mirror.
type ComposeOutput struct {
	Output string `json:"output"`
}

type FileWriteRequest struct {
	ID             string `json:"operation_id"`
	ProjectUID     string `json:"-"`
	RelativePath   string `json:"relative_path"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Content        string `json:"content"`
}

type Backup struct {
	ID             string    `json:"backup_id"`
	ProjectUID     string    `json:"project_uid"`
	CreatedAt      time.Time `json:"created_at"`
	Trigger        string    `json:"trigger"`
	FileCount      int       `json:"file_count"`
	SizeBytes      int64     `json:"size_bytes"`
	ManifestSHA256 string    `json:"manifest_sha256"`
}

type BackupCreateRequest struct {
	ID            string   `json:"operation_id"`
	ProjectUID    string   `json:"-"`
	RelativePaths []string `json:"relative_paths"`
}

type BackupRestoreRequest struct {
	ID         string `json:"operation_id"`
	ProjectUID string `json:"-"`
	BackupID   string `json:"-"`
}

type Operation struct {
	ID                     string `json:"operation_id"`
	Status                 string `json:"status"`
	Phase                  string `json:"phase"`
	Revision               uint64 `json:"revision"`
	PartialEffectsPossible bool   `json:"partial_effects_possible"`
	Error                  string `json:"error,omitempty"`
	OutputTail             string `json:"output_tail,omitempty"`
	OutputTruncated        bool   `json:"output_truncated"`
}

type OperationCancellation struct {
	Outcome   string    `json:"outcome"`
	Operation Operation `json:"operation"`
}

type EnvironmentEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

type LiveRequest struct {
	AgentID     string
	ContainerID string
	Follow      bool
	TailLines   uint64
	ShowStdout  bool
	ShowStderr  bool
	Timestamps  bool
}

// ProjectLogRequest selects a discovered project and optional discovered
// services. It intentionally has no Agent ID, container ID, or raw CLI flag.
type ProjectLogRequest struct {
	Services   []string
	Follow     bool
	TailLines  uint64
	Timestamps bool
}

type LogEvent struct {
	Data         []byte    `json:"data,omitempty"`
	Stream       string    `json:"stream,omitempty"`
	LineCount    uint64    `json:"line_count,omitempty"`
	Timestamp    time.Time `json:"timestamp,omitempty"`
	DroppedBytes uint64    `json:"dropped_bytes,omitempty"`
	DroppedLines uint64    `json:"dropped_lines,omitempty"`
	Terminal     bool      `json:"terminal,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type StatsSample struct {
	ContainerID  string        `json:"container_id"`
	ObservedAt   time.Time     `json:"observed_at"`
	CPUPercent   float64       `json:"cpu_percent"`
	MemoryUsage  uint64        `json:"memory_usage"`
	MemoryLimit  uint64        `json:"memory_limit"`
	NetworkRX    uint64        `json:"network_rx"`
	NetworkTX    uint64        `json:"network_tx"`
	BlockRead    uint64        `json:"block_read"`
	BlockWrite   uint64        `json:"block_write"`
	RestartCount uint64        `json:"restart_count"`
	Health       string        `json:"health,omitempty"`
	Uptime       time.Duration `json:"uptime_nano"`
}

type LogStream interface {
	Recv(context.Context) (LogEvent, error)
	Close() error
}

type StatsStream interface {
	Recv(context.Context) (StatsSample, error)
	Close() error
}

// Backend is intentionally snapshot-oriented. Implementations may query an
// active Agent session, but must not mirror Docker state, logs, metrics, or
// configuration contents into Server persistence.
type Backend interface {
	Dashboard(context.Context) (Dashboard, error)
	Host(context.Context, string) (Host, error)
	HostContainers(context.Context, string) ([]HostContainer, error)
	HostImages(context.Context, string) ([]HostImage, error)
	HostNetworks(context.Context, string) ([]HostNetwork, error)
	HostVolumes(context.Context, string) ([]HostVolume, error)
	HostAudit(context.Context, string, AuditPageRequest) (AuditPage, error)
	ProjectActivity(context.Context, string, AuditPageRequest) (AuditPage, error)
	ProjectEnvironment(context.Context, string) ([]EnvironmentEntry, error)
	ProjectComposePS(context.Context, string, ComposeQuery) (ComposeOutput, error)
	ProjectComposeConfig(context.Context, string, ComposeQuery) (ComposeOutput, error)
	ProjectFile(context.Context, string, string) (ProjectFile, error)
	WriteProjectFile(context.Context, FileWriteRequest) (Operation, error)
	ProjectBackups(context.Context, string) ([]Backup, error)
	CreateBackup(context.Context, BackupCreateRequest) (Operation, error)
	RestoreBackup(context.Context, BackupRestoreRequest) (Operation, error)
	StartOperation(context.Context, OperationRequest) (Operation, error)
	GetOperation(context.Context, string, string) (Operation, error)
	CancelOperation(context.Context, string, string) (OperationCancellation, error)
	OpenLogs(context.Context, LiveRequest) (LogStream, error)
	OpenProjectLogs(context.Context, string, ProjectLogRequest) (LogStream, error)
	OpenStats(context.Context, LiveRequest) (StatsStream, error)
}
