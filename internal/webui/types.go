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
	Connection Capability `json:"connection"`
	Docker     Capability `json:"docker"`
	Compose    Capability `json:"compose"`
	Discovery  Capability `json:"discovery"`
	// Metrics is where a host says it cannot serve the live metrics matrix, and
	// why. It belongs here rather than only on the stream's error because a
	// console needs the answer before it opens a stream, and because this is
	// where every other capability reason already appears.
	Metrics           Capability `json:"metrics"`
	OperationRecovery Capability `json:"operation_recovery"`
	FSRead            Capability `json:"fs_read"`
	FSWrite           Capability `json:"fs_write"`
}

type Host struct {
	ID                   string         `json:"id"`
	DisplayName          string         `json:"display_name"`
	State                string         `json:"state"`
	Capabilities         Capabilities   `json:"capabilities"`
	ProjectScan          *ProjectScan   `json:"project_scan,omitempty"`
	SessionSourceIP      string         `json:"session_source_ip,omitempty"`
	SessionObservedAt    *time.Time     `json:"session_observed_at,omitempty"`
	DockerAPIVersion     string         `json:"docker_api_version,omitempty"`
	DockerComposeVersion string         `json:"docker_compose_version,omitempty"`
	EngineSummary        *EngineSummary `json:"engine_summary,omitempty"`
	EngineSummaryReason  string         `json:"engine_summary_reason,omitempty"`
}

type EngineSummary struct {
	Version           string `json:"version,omitempty"`
	CPUCapacity       uint32 `json:"cpu_capacity"`
	MemoryCapacity    uint64 `json:"memory_capacity_bytes"`
	ContainersTotal   uint32 `json:"containers_total"`
	ContainersRunning uint32 `json:"containers_running"`
	Images            uint32 `json:"images"`
	StorageDriver     string `json:"storage_driver,omitempty"`
	LoggingDriver     string `json:"logging_driver,omitempty"`
	CgroupDriver      string `json:"cgroup_driver,omitempty"`
	CgroupVersion     string `json:"cgroup_version,omitempty"`
	DefaultRuntime    string `json:"default_runtime,omitempty"`
	OperatingSystem   string `json:"operating_system,omitempty"`
	OSVersion         string `json:"os_version,omitempty"`
	OSType            string `json:"os_type,omitempty"`
	Architecture      string `json:"architecture,omitempty"`
	KernelVersion     string `json:"kernel_version,omitempty"`
	DockerRootDir     string `json:"docker_root_dir,omitempty"`
}

// HostContainer, HostImage, HostNetwork, and HostVolume are deliberately
// curated live views. Lists stay minimal; the explicit Inspector methods add
// bounded diagnostic fields without exposing raw Docker inspect payloads.
type HostContainer struct {
	ID                  string             `json:"id"`
	Names               []string           `json:"names"`
	Image               string             `json:"image"`
	State               string             `json:"state"`
	Status              string             `json:"status"`
	Health              string             `json:"health,omitempty"`
	ComposeProject      string             `json:"compose_project,omitempty"`
	ComposeService      string             `json:"compose_service,omitempty"`
	OneOff              bool               `json:"one_off"`
	Orphan              bool               `json:"orphan"`
	Ports               []PublishedPort    `json:"ports"`
	Protected           bool               `json:"protected"`
	ProtectionReason    string             `json:"protection_reason,omitempty"`
	ImageID             string             `json:"image_id,omitempty"`
	ExitCode            int                `json:"exit_code"`
	CreatedAt           string             `json:"created_at,omitempty"`
	StartedAt           string             `json:"started_at,omitempty"`
	FinishedAt          string             `json:"finished_at,omitempty"`
	OOMKilled           bool               `json:"oom_killed"`
	RestartCount        int                `json:"restart_count"`
	RestartPolicy       string             `json:"restart_policy,omitempty"`
	RestartMaximumRetry int                `json:"restart_maximum_retry"`
	StopSignal          string             `json:"stop_signal,omitempty"`
	StopTimeout         *int               `json:"stop_timeout_seconds,omitempty"`
	LoggingDriver       string             `json:"logging_driver,omitempty"`
	Command             []string           `json:"command,omitempty"`
	Entrypoint          []string           `json:"entrypoint,omitempty"`
	ExposedPorts        []string           `json:"exposed_ports,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Mounts              []ContainerMount   `json:"mounts,omitempty"`
	Networks            []ContainerNetwork `json:"networks,omitempty"`
}

type ContainerMount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}
type ContainerNetwork struct {
	Name       string   `json:"name"`
	NetworkID  string   `json:"network_id,omitempty"`
	EndpointID string   `json:"endpoint_id,omitempty"`
	IPv4       string   `json:"ipv4,omitempty"`
	IPv6       string   `json:"ipv6,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

type PublishedPort struct {
	HostIP        string `json:"host_ip,omitempty"`
	PublishedPort uint16 `json:"published_port,omitempty"`
	TargetPort    uint16 `json:"target_port"`
	Protocol      string `json:"protocol"`
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

type ObjectReference struct {
	ContainerID    string `json:"container_id"`
	ContainerName  string `json:"container_name,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	State          string `json:"state,omitempty"`
	Destination    string `json:"destination,omitempty"`
}
type NetworkAttachment struct {
	ObjectReference
	EndpointID string `json:"endpoint_id,omitempty"`
	IPv4       string `json:"ipv4,omitempty"`
	IPv6       string `json:"ipv6,omitempty"`
	MAC        string `json:"mac,omitempty"`
}
type HostImageDetails struct {
	ID           string            `json:"id"`
	RepoTags     []string          `json:"repo_tags"`
	RepoDigests  []string          `json:"repo_digests"`
	Created      string            `json:"created,omitempty"`
	Author       string            `json:"author,omitempty"`
	Architecture string            `json:"architecture,omitempty"`
	Variant      string            `json:"variant,omitempty"`
	OS           string            `json:"os,omitempty"`
	OSVersion    string            `json:"os_version,omitempty"`
	Size         int64             `json:"size_bytes"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	Command      []string          `json:"command,omitempty"`
	ExposedPorts []string          `json:"exposed_ports,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	User         string            `json:"user,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LayerCount   int               `json:"layer_count"`
	UsedBy       []ObjectReference `json:"used_by"`
}
type IPAMConfig struct {
	Subnet       string            `json:"subnet,omitempty"`
	IPRange      string            `json:"ip_range,omitempty"`
	Gateway      string            `json:"gateway,omitempty"`
	AuxAddresses map[string]string `json:"aux_addresses,omitempty"`
}
type HostNetworkDetails struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Created        string              `json:"created,omitempty"`
	Scope          string              `json:"scope"`
	Driver         string              `json:"driver"`
	EnableIPv4     bool                `json:"enable_ipv4"`
	EnableIPv6     bool                `json:"enable_ipv6"`
	Internal       bool                `json:"internal"`
	Attachable     bool                `json:"attachable"`
	Ingress        bool                `json:"ingress"`
	ConfigOnly     bool                `json:"config_only"`
	IPAMDriver     string              `json:"ipam_driver,omitempty"`
	IPAM           []IPAMConfig        `json:"ipam"`
	Options        map[string]string   `json:"options,omitempty"`
	Labels         map[string]string   `json:"labels,omitempty"`
	ComposeProject string              `json:"compose_project,omitempty"`
	ComposeNetwork string              `json:"compose_network,omitempty"`
	Attachments    []NetworkAttachment `json:"attachments"`
}
type HostVolumeDetails struct {
	Name           string            `json:"name"`
	Driver         string            `json:"driver"`
	Scope          string            `json:"scope"`
	CreatedAt      string            `json:"created_at,omitempty"`
	Mountpoint     string            `json:"mountpoint,omitempty"`
	Options        map[string]string `json:"options,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	ComposeProject string            `json:"compose_project,omitempty"`
	ComposeVolume  string            `json:"compose_volume,omitempty"`
	References     []ObjectReference `json:"references"`
}

type ProjectScan struct {
	ScannedAt       time.Time `json:"scanned_at"`
	Truncated       bool      `json:"truncated"`
	StopReason      string    `json:"stop_reason,omitempty"`
	DirectoriesSeen int       `json:"directories_seen"`
	LastScannedPath string    `json:"last_scanned_path,omitempty"`
}

type Project struct {
	UID                 string             `json:"uid"`
	AgentID             string             `json:"agent_id"`
	WorkingDir          string             `json:"working_dir"`
	Name                string             `json:"name"`
	Managed             bool               `json:"managed"`
	UnmanagedReason     string             `json:"unmanaged_reason,omitempty"`
	ContainerIDs        []string           `json:"container_ids,omitempty"`
	Services            []string           `json:"services,omitempty"`
	ComposeFiles        []string           `json:"compose_files,omitempty"`
	DefinedServices     []ComposeService   `json:"defined_services,omitempty"`
	ActiveProfiles      []string           `json:"active_profiles,omitempty"`
	EnvFiles            []EnvFileReference `json:"env_files,omitempty"`
	Secrets             []ComposeResource  `json:"secrets,omitempty"`
	Configs             []ComposeResource  `json:"configs,omitempty"`
	PullServices        []string           `json:"pull_services,omitempty"`
	ProjectUpAvailable  bool               `json:"project_up_available"`
	ProjectUpReason     string             `json:"project_up_reason,omitempty"`
	IncludedBy          []string           `json:"included_by,omitempty"`
	SourceReferences    []SourceReference  `json:"source_references,omitempty"`
	SourceGraphComplete bool               `json:"source_graph_complete"`
	Present             bool               `json:"present"`
	Stale               bool               `json:"stale"`
	ReadOnly            bool               `json:"read_only"`
	Collision           bool               `json:"collision"`
	ComposeExecutable   bool               `json:"compose_executable"`
	FilesystemWritable  bool               `json:"filesystem_writable"`
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
	LastObservedAt          *time.Time `json:"last_observed_at,omitempty"`
	Drift                   string     `json:"drift"`
}

type ComposeService struct {
	Name              string   `json:"name"`
	Image             string   `json:"image,omitempty"`
	HasBuild          bool     `json:"has_build"`
	PullPolicy        string   `json:"pull_policy,omitempty"`
	Profiles          []string `json:"profiles,omitempty"`
	DependsOn         []string `json:"depends_on,omitempty"`
	Active            bool     `json:"active"`
	BuildRequired     bool     `json:"build_required"`
	PullAvailable     bool     `json:"pull_available"`
	UpAvailable       bool     `json:"up_available"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type ProjectRuntime struct {
	ProjectUID string           `json:"project_uid"`
	ObservedAt *time.Time       `json:"observed_at,omitempty"`
	Services   []ServiceRuntime `json:"services"`
	Orphans    []HostContainer  `json:"orphans"`
}

type ServiceRuntime struct {
	Name            string          `json:"name"`
	Status          string          `json:"status"`
	ProfileInactive bool            `json:"profile_inactive"`
	Containers      []HostContainer `json:"containers"`
}

type EnvFileReference struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
}

type ComposeResource struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type,omitempty"`
	Source     string `json:"source,omitempty"`
	External   bool   `json:"external"`
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
	Cursor   *AuditCursor `json:"-"`
	Limit    int          `json:"-"`
	From     *time.Time   `json:"-"`
	Until    *time.Time   `json:"-"`
	Resource string       `json:"-"`
	Kind     string       `json:"-"`
	Actor    string       `json:"-"`
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
	Reveal   bool     `json:"reveal"`
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
	Paths          []string  `json:"paths"`
	PathsAvailable bool      `json:"paths_available"`
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
	ID                     string     `json:"operation_id"`
	AgentID                string     `json:"agent_id"`
	ProjectUID             string     `json:"project_uid,omitempty"`
	Kind                   string     `json:"kind"`
	Target                 string     `json:"target,omitempty"`
	Status                 string     `json:"status"`
	Phase                  string     `json:"phase"`
	Revision               uint64     `json:"revision"`
	CancelMode             string     `json:"cancel_mode,omitempty"`
	CanCancel              bool       `json:"can_cancel"`
	CancelabilityReason    string     `json:"cancelability_reason,omitempty"`
	RequestedAt            *time.Time `json:"requested_at,omitempty"`
	StartedAt              *time.Time `json:"started_at,omitempty"`
	FinishedAt             *time.Time `json:"finished_at,omitempty"`
	PartialEffectsPossible bool       `json:"partial_effects_possible"`
	Error                  string     `json:"error,omitempty"`
	OutputTail             string     `json:"output_tail,omitempty"`
	OutputTruncated        bool       `json:"output_truncated"`
}

type OperationListRequest struct {
	Limit int
}

type OperationList struct {
	Operations []Operation `json:"operations"`
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
	Since       time.Time
	Until       time.Time
}

// ProjectLogRequest selects a discovered project and optional discovered
// services. It intentionally has no Agent ID, container ID, or raw CLI flag.
type ProjectLogRequest struct {
	ContainerID string
	Services    []string
	Follow      bool
	TailLines   uint64
	Timestamps  bool
	Since       time.Time
	Until       time.Time
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

// The matrix view types below are the browser's whole picture of one host at
// one instant. They are a rendering shape, not a second model: the Server has
// already decided membership, context and aggregation, and nothing in this
// package recomputes any of it.
//
// MatrixTotals is what a group of containers adds up to, and appears
// identically on the host, project and service rows, because those rows are all
// projections of the same container samples in the same frame.
type MatrixTotals struct {
	// ContainerCount is how many containers the row covers and PendingCount how
	// many of those have not reported a sample yet. A row whose numbers come
	// from fewer containers than it covers says so with these two.
	ContainerCount uint32 `json:"container_count"`
	PendingCount   uint32 `json:"pending_count"`

	CPUPercent  float64 `json:"cpu_percent"`
	MemoryUsage uint64  `json:"memory_usage"`
	NetworkRX   uint64  `json:"network_rx"`
	NetworkTX   uint64  `json:"network_tx"`
	BlockRead   uint64  `json:"block_read"`
	BlockWrite  uint64  `json:"block_write"`
	Restarts    uint64  `json:"restarts"`

	// MemoryLimit is present only when every member has a limit.
	// MemoryLimitUnbounded means at least one member runs without one, which is
	// not a large number but a different kind of answer; the percent is
	// withheld rather than computed against something that bounds nothing.
	MemoryLimit          uint64  `json:"memory_limit,omitempty"`
	MemoryLimitUnbounded bool    `json:"memory_limit_unbounded"`
	MemoryPercent        float64 `json:"memory_percent,omitempty"`
	MemoryPercentKnown   bool    `json:"memory_percent_known"`

	// Health is the worst status the members answered: "unhealthy" if any is,
	// "starting" if none is unhealthy and any is starting, "healthy" if every
	// member that has a healthcheck reports healthy, "none" if the row has
	// members and not one has a healthcheck, and absent when every member is
	// still pending. HealthUnreported counts members that answered nothing, so
	// a row reading healthy alongside a nonzero count is saying that every
	// container which answers is healthy and this many did not answer.
	Health           string `json:"health,omitempty"`
	HealthUnreported uint32 `json:"health_unreported"`

	// Uptime is the youngest member's, because a row is only as old as its
	// newest container.
	Uptime      time.Duration `json:"uptime_nano,omitempty"`
	UptimeKnown bool          `json:"uptime_known"`
}

// MatrixFilesystem is capacity for one path Dockpilot writes to. Unavailable is
// a fact about that path, not about the host.
type MatrixFilesystem struct {
	Path        string `json:"path"`
	TotalBytes  uint64 `json:"total_bytes"`
	FreeBytes   uint64 `json:"free_bytes"`
	Unavailable bool   `json:"unavailable"`
	Reason      string `json:"reason,omitempty"`
}

// MatrixHostRow is the Docker workload an Agent manages, against the capacity
// its Engine reports. It is deliberately not host OS metrics, and a console
// must not label it as such: CPUCapacity and MemoryCapacity are what the
// machine has, Totals is what Dockpilot's containers are using, and what is
// excluded from that is stated rather than papered over.
type MatrixHostRow struct {
	CPUCapacity       uint32             `json:"cpu_capacity"`
	MemoryCapacity    uint64             `json:"memory_capacity"`
	ContainersRunning uint32             `json:"containers_running"`
	ContainersTotal   uint32             `json:"containers_total"`
	Filesystems       []MatrixFilesystem `json:"filesystems"`
	Totals            MatrixTotals       `json:"totals"`
}

// MatrixContainer is one container row. Pending is a member whose first sample
// has not arrived, which is a different state from gone and is shown as one.
// Unmapped is a container belonging to no project - it keeps its metrics and is
// never hidden, because the Engine decides what is running and this mapping is
// only context.
type MatrixContainer struct {
	ContainerID string      `json:"container_id"`
	Pending     bool        `json:"pending"`
	Unmapped    bool        `json:"unmapped"`
	ProjectUID  string      `json:"project_uid,omitempty"`
	ProjectName string      `json:"project_name,omitempty"`
	Service     string      `json:"service,omitempty"`
	Image       string      `json:"image,omitempty"`
	Sample      StatsSample `json:"sample"`

	MemoryLimitUnbounded bool    `json:"memory_limit_unbounded"`
	MemoryPercent        float64 `json:"memory_percent,omitempty"`
	MemoryPercentKnown   bool    `json:"memory_percent_known"`
}

type MatrixService struct {
	Service    string            `json:"service"`
	Unmapped   bool              `json:"unmapped"`
	Totals     MatrixTotals      `json:"totals"`
	Containers []MatrixContainer `json:"containers"`
}

type MatrixProject struct {
	ProjectUID  string          `json:"project_uid,omitempty"`
	ProjectName string          `json:"project_name,omitempty"`
	Unmapped    bool            `json:"unmapped"`
	Totals      MatrixTotals    `json:"totals"`
	Services    []MatrixService `json:"services"`
}

// MatrixFrame is one host at one instant.
//
// The three staleness flags travel from the Agent unchanged and are not
// normalized here. Each says that one part of the frame is the last thing that
// could be confirmed rather than the current thing, and they move
// independently: listing containers, asking the Engine about itself, and
// looking up project context are different calls that fail for different
// reasons. A console that collapses them will tell an operator that Docker is
// down when only a project name is missing.
type MatrixFrame struct {
	AgentID    string          `json:"agent_id"`
	ObservedAt time.Time       `json:"observed_at"`
	Host       MatrixHostRow   `json:"host"`
	Projects   []MatrixProject `json:"projects"`

	// AgentDroppedFrames is what the Agent discarded because this Server was
	// slow to read; ServerDroppedFrames is what this Server discarded because
	// this browser was slow to read. They are different failures with different
	// fixes and are never added together.
	AgentDroppedFrames  uint64 `json:"agent_dropped_frames"`
	ServerDroppedFrames uint64 `json:"server_dropped_frames"`

	MembershipStale  bool   `json:"membership_stale"`
	MembershipReason string `json:"membership_reason,omitempty"`
	WorkloadStale    bool   `json:"workload_stale"`
	WorkloadReason   string `json:"workload_reason,omitempty"`
	ContextStale     bool   `json:"context_stale"`
	ContextReason    string `json:"context_reason,omitempty"`
}

type MatrixStream interface {
	Recv(context.Context) (MatrixFrame, error)
	Close() error
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
	HostContainer(context.Context, string, string) (HostContainer, error)
	HostImages(context.Context, string) ([]HostImage, error)
	HostImage(context.Context, string, string) (HostImageDetails, error)
	HostNetworks(context.Context, string) ([]HostNetwork, error)
	HostNetwork(context.Context, string, string) (HostNetworkDetails, error)
	HostVolumes(context.Context, string) ([]HostVolume, error)
	HostVolume(context.Context, string, string) (HostVolumeDetails, error)
	HostAudit(context.Context, string, AuditPageRequest) (AuditPage, error)
	ProjectActivity(context.Context, string, AuditPageRequest) (AuditPage, error)
	ProjectEnvironment(context.Context, string) ([]EnvironmentEntry, error)
	ProjectRuntime(context.Context, string) (ProjectRuntime, error)
	ProjectComposePS(context.Context, string, ComposeQuery) (ComposeOutput, error)
	ProjectComposeConfig(context.Context, string, ComposeQuery) (ComposeOutput, error)
	ProjectFile(context.Context, string, string) (ProjectFile, error)
	WriteProjectFile(context.Context, FileWriteRequest) (Operation, error)
	ProjectBackups(context.Context, string) ([]Backup, error)
	CreateBackup(context.Context, BackupCreateRequest) (Operation, error)
	RestoreBackup(context.Context, BackupRestoreRequest) (Operation, error)
	StartOperation(context.Context, OperationRequest) (Operation, error)
	ListOperations(context.Context, OperationListRequest) (OperationList, error)
	GetOperation(context.Context, string, string) (Operation, error)
	CancelOperation(context.Context, string, string) (OperationCancellation, error)
	OpenLogs(context.Context, LiveRequest) (LogStream, error)
	OpenProjectLogs(context.Context, string, ProjectLogRequest) (LogStream, error)
	OpenStats(context.Context, LiveRequest) (StatsStream, error)
	// OpenMatrix attaches one viewer to a host's live metrics. Every viewer of
	// a host shares one Agent stream; the implementation owns that sharing, and
	// this package only encodes what arrives.
	OpenMatrix(context.Context, string) (MatrixStream, error)
}
