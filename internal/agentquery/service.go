// Package agentquery implements the Agent's bounded, read-only product query
// surface. Query results are transient and are never cached or persisted.
package agentquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/safefile"
)

const (
	QueryContainerList      = "container.list"
	QueryContainerInspect   = "container.inspect"
	QueryImageList          = "image.list"
	QueryImageInspect       = "image.inspect"
	QueryNetworkList        = "network.list"
	QueryNetworkInspect     = "network.inspect"
	QueryVolumeList         = "volume.list"
	QueryVolumeInspect      = "volume.inspect"
	QueryEngineInfo         = "engine.info"
	QueryProjectList        = "project.list"
	QueryProjectSnapshot    = "project.snapshot"
	QueryProjectStatus      = "project.status"
	QueryFileRead           = "file.read"
	QueryProjectEnvironment = "project_environment"
	QueryBackupList         = "backup.list"
	QueryComposePS          = "compose.ps"
	QueryComposeConfig      = "compose.config"

	maxRequestPayloadBytes = 64 << 10
	// gRPC's message limit includes the protobuf field tag and length prefix,
	// not only QueryResponse.payload. Keep a small explicit envelope reserve.
	maxResponsePayloadBytes = producttransport.DefaultMaxMessageBytes - 16
	maxInventoryItems       = 10_000
	maxProjectDockerFacts   = 4_096
	maxProjectSourceRefs    = 512
	maxImageReferences      = 256
	maxComposeQueryBytes    = 128 << 10
)

var (
	ErrInvalidRequest     = errors.New("invalid Agent query request")
	ErrUnsupportedQuery   = errors.New("unsupported Agent query")
	ErrResponseTooLarge   = errors.New("Agent query response exceeds 1 MiB")
	ErrProjectUnavailable = errors.New("Agent project is unavailable")

	canonicalID  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	safeBackupID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	serviceName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	volumeName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

type DockerReader interface {
	List(context.Context) ([]dockeradapter.Container, error)
	Inspect(context.Context, string) (dockeradapter.Container, error)
}

type DockerInventoryReader interface {
	ListImages(context.Context) ([]dockeradapter.Image, error)
	ListNetworks(context.Context) ([]dockeradapter.Network, error)
	ListVolumes(context.Context) ([]dockeradapter.Volume, error)
}

type DockerInfoReader interface {
	Info(context.Context) (dockeradapter.EngineInfo, error)
}

type DockerDetailReader interface {
	InspectImage(context.Context, string) (dockeradapter.ImageDetails, error)
	InspectNetwork(context.Context, string) (dockeradapter.NetworkDetails, error)
	InspectVolume(context.Context, string) (dockeradapter.VolumeDetails, error)
}

type ProjectCatalog interface {
	Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus)
	ProjectSnapshot(string) (agentprojects.Project, bool)
	Project(context.Context, string) (composeexec.Project, bool, error)
}

// ComposeRunner is the same fixed-argv boundary used by mutation operations.
// Query code receives no raw executable path or arbitrary arguments.
type ComposeRunner interface {
	Run(context.Context, composeexec.Spec, chan<- composeexec.OutputChunk) (composeexec.Result, error)
}

type FileReader interface {
	Read(context.Context, string, string) (safefile.File, error)
}

// BackupMetadataLister is intentionally metadata-only. Archive paths,
// manifests, and configuration bytes never cross this boundary.
//
// RecoveryBlocked reports a project whose changes the Agent is refusing because
// a restore failed and its rollback failed too. It belongs here rather than in
// the project catalog because the restore journal owns that state, and it is
// reported because a Server that does not know about it advertises a project as
// writable that every mutation will refuse.
type BackupMetadataLister interface {
	List(context.Context, string) ([]backup.Metadata, error)
	LoadManifest(string, string) (backup.Manifest, error)
	RecoveryBlocked(projectUID string) bool
}

var _ BackupMetadataLister = (*backup.Manager)(nil)

type Config struct {
	Docker   DockerReader
	Projects ProjectCatalog
	Files    FileReader
	Backups  BackupMetadataLister
	Compose  ComposeRunner
}

type Service struct{ config Config }

var _ producttransport.QueryHandler = (*Service)(nil)

func New(config Config) (*Service, error) {
	if config.Docker == nil || config.Projects == nil || config.Files == nil || config.Backups == nil || config.Compose == nil {
		return nil, errors.New("agentquery: Docker, project, safe-file, backup, and Compose readers are required")
	}
	return &Service{config: config}, nil
}

type FileReadRequest struct {
	RelativePath string `json:"relative_path"`
}

type FileResponse struct {
	RelativePath string               `json:"relative_path"`
	Content      string               `json:"content"`
	SHA256       string               `json:"sha256"`
	MTime        time.Time            `json:"mtime"`
	Mode         uint32               `json:"mode"`
	LineEndings  safefile.LineEndings `json:"line_endings"`
	Secret       bool                 `json:"secret"`
}

type EnvironmentEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

type ProjectListResponse struct {
	Projects    []Project           `json:"projects"`
	DockerFacts []DockerProjectFact `json:"docker_facts"`
	Status      ScanStatus          `json:"status"`
}

// DockerProjectFact is a raw observation of public Compose labels on one
// container. The Agent does not derive a project identity or interpret the
// config hash; the Server joins this with filesystem facts by working_dir.
type DockerProjectFact struct {
	ContainerID string   `json:"container_id"`
	ProjectName string   `json:"project_name"`
	WorkingDir  string   `json:"working_dir,omitempty"`
	ConfigFiles []string `json:"config_files,omitempty"`
	Service     string   `json:"service,omitempty"`
	ConfigHash  string   `json:"config_hash,omitempty"`
}

// ProjectSnapshotResponse carries one post-operation observation. Keeping it
// separate from the full project-list query prevents a targeted rescan from
// implying that every project on the host was freshly verified.
type ProjectSnapshotResponse struct {
	Project Project `json:"project"`
}

type Container struct {
	ID                  string             `json:"id"`
	Names               []string           `json:"names"`
	Image               string             `json:"image"`
	State               string             `json:"state"`
	Status              string             `json:"status"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Mounts              []Mount            `json:"mounts"`
	Health              string             `json:"health,omitempty"`
	ComposeProject      string             `json:"compose_project,omitempty"`
	ComposeService      string             `json:"compose_service,omitempty"`
	OneOff              bool               `json:"one_off"`
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
	Networks            []ContainerNetwork `json:"networks,omitempty"`
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

type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

type Image struct {
	ID          string   `json:"id"`
	RepoTags    []string `json:"repo_tags"`
	RepoDigests []string `json:"repo_digests"`
	Created     int64    `json:"created_unix"`
	Size        int64    `json:"size_bytes"`
	Containers  int64    `json:"containers"`
}

type Network struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Scope      string `json:"scope"`
	Internal   bool   `json:"internal"`
	Attachable bool   `json:"attachable"`
	Ingress    bool   `json:"ingress"`
}

type Volume struct {
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

type ImageDetails struct {
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

type NetworkDetails struct {
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

type VolumeDetails struct {
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

// EngineSummary is a bounded, one-shot Engine snapshot. It intentionally
// stays separate from the viewer-scoped live metrics matrix.
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

type Project struct {
	UID                 string             `json:"project_uid"`
	Root                string             `json:"root"`
	WorkingDir          string             `json:"working_dir"`
	Files               []FileFact         `json:"files"`
	Name                string             `json:"name"`
	Services            []string           `json:"services"`
	ComposeFiles        []string           `json:"compose_files"`
	DefinedServices     []ServiceModel     `json:"defined_services"`
	ActiveProfiles      []string           `json:"active_profiles,omitempty"`
	EnvFiles            []EnvFileReference `json:"env_files,omitempty"`
	Secrets             []ResourceSource   `json:"secrets,omitempty"`
	Configs             []ResourceSource   `json:"configs,omitempty"`
	PullServices        []string           `json:"pull_services,omitempty"`
	ProjectUpAvailable  bool               `json:"project_up_available"`
	ProjectUpReason     string             `json:"project_up_reason,omitempty"`
	IncludedWorkDirs    []string           `json:"included_work_dirs,omitempty"`
	SourceReferences    []SourceReference  `json:"source_references,omitempty"`
	SourceGraphComplete bool               `json:"source_graph_complete"`
	CurrentFingerprint  string             `json:"current_fingerprint"`
	ComposeExecutable   bool               `json:"compose_executable"`
	FilesystemWritable  bool               `json:"filesystem_writable"`
	CapabilityReason    string             `json:"capability_reason,omitempty"`
	Stale               bool               `json:"stale"`
	// RestoreRecoveryRequired is set when a failed restore could not be rolled
	// back. The project's files are in an unknown state, the Agent refuses to
	// change them, and an operator has to resolve it by hand.
	RestoreRecoveryRequired bool `json:"restore_recovery_required,omitempty"`
}

type ServiceModel struct {
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

type EnvFileReference struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
}

type ResourceSource struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type,omitempty"`
	Source     string `json:"source,omitempty"`
	External   bool   `json:"external"`
}

// SourceReference is content-free Compose include/extends provenance. It is
// metadata only: file reads still go through the Agent's safefile boundary.
type SourceReference struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	ReadOnly   bool   `json:"read_only"`
}

type FileFact struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ScanStatus struct {
	ScannedAt       time.Time `json:"scanned_at"`
	Truncated       bool      `json:"truncated"`
	StopReason      string    `json:"stop_reason,omitempty"`
	DirectoriesSeen int       `json:"directories_seen"`
	LastScannedPath string    `json:"last_scanned_path,omitempty"`
}

type BackupMetadata struct {
	BackupID       string         `json:"backup_id"`
	ProjectUID     string         `json:"project_uid"`
	CreatedAt      time.Time      `json:"created_at"`
	Trigger        backup.Trigger `json:"trigger"`
	FileCount      int            `json:"file_count"`
	SizeBytes      int64          `json:"size_bytes"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	Paths          []string       `json:"paths"`
}

// ComposePSRequest and ComposeConfigRequest are closed, project-scoped query
// envelopes. They cannot carry filesystem paths or arbitrary Compose flags.
type ComposePSRequest struct {
	Services []string `json:"services"`
	All      bool     `json:"all"`
}

type ComposeConfigRequest struct {
	Services []string `json:"services"`
	Reveal   bool     `json:"reveal,omitempty"`
}

// ComposeOutput is transient CLI text. The Agent does not cache or persist it
// and rejects results that exceed the fixed capture limit.
type ComposeOutput struct {
	Output string `json:"output"`
}

func (s *Service) Query(ctx context.Context, _ producttransport.SessionInfo, request producttransport.QueryRequest) (producttransport.QueryResponse, error) {
	if err := validateEnvelope(request); err != nil {
		return producttransport.QueryResponse{}, err
	}
	var value any
	var err error
	switch request.Kind {
	case QueryContainerList:
		if err = requireEmpty(request); err == nil {
			value, err = s.containerList(ctx)
		}
	case QueryContainerInspect:
		if err = requireTargetOnly(request); err == nil && !canonicalID.MatchString(request.Target) {
			err = fmt.Errorf("%w: container target must be a canonical 64-character ID", ErrInvalidRequest)
		}
		if err == nil {
			var inspected dockeradapter.Container
			inspected, err = s.config.Docker.Inspect(ctx, request.Target)
			value = containerResponse(inspected)
		}
	case QueryImageList:
		if err = requireEmpty(request); err == nil {
			value, err = s.imageList(ctx)
		}
	case QueryImageInspect:
		if err = requireTargetOnly(request); err == nil {
			value, err = s.imageInspect(ctx, request.Target)
		}
	case QueryNetworkList:
		if err = requireEmpty(request); err == nil {
			value, err = s.networkList(ctx)
		}
	case QueryNetworkInspect:
		if err = requireTargetOnly(request); err == nil {
			value, err = s.networkInspect(ctx, request.Target)
		}
	case QueryVolumeList:
		if err = requireEmpty(request); err == nil {
			value, err = s.volumeList(ctx)
		}
	case QueryVolumeInspect:
		if err = requireTargetOnly(request); err == nil {
			value, err = s.volumeInspect(ctx, request.Target)
		}
	case QueryEngineInfo:
		if err = requireEmpty(request); err == nil {
			value, err = s.engineInfo(ctx)
		}
	case QueryProjectList:
		if err = requireEmpty(request); err == nil {
			projects, status := s.config.Projects.Snapshot()
			var containers []dockeradapter.Container
			containers, err = s.config.Docker.List(ctx)
			if err == nil {
				value, err = projectListResponse(projects, status, containers, s.config.Backups.RecoveryBlocked)
			}
		}
	case QueryProjectSnapshot:
		if err = requireTargetOnly(request); err == nil {
			err = requireProjectTarget(request)
		}
		if err == nil {
			var project agentprojects.Project
			var found bool
			project, found = s.config.Projects.ProjectSnapshot(request.Target)
			if !found {
				err = ErrProjectUnavailable
			} else {
				value = ProjectSnapshotResponse{Project: projectResponse(project, s.config.Backups.RecoveryBlocked(project.UID))}
			}
		}
	case QueryProjectStatus:
		if err = requireEmpty(request); err == nil {
			_, status := s.config.Projects.Snapshot()
			value = scanStatusResponse(status)
		}
	case QueryFileRead:
		value, err = s.fileRead(ctx, request)
	case QueryProjectEnvironment:
		value, err = s.projectEnvironment(ctx, request)
	case QueryBackupList:
		value, err = s.backupList(ctx, request)
	case QueryComposePS:
		value, err = s.composePS(ctx, request)
	case QueryComposeConfig:
		value, err = s.composeConfig(ctx, request)
	default:
		return producttransport.QueryResponse{}, fmt.Errorf("%w: %q", ErrUnsupportedQuery, request.Kind)
	}
	if err != nil {
		return producttransport.QueryResponse{}, err
	}
	return marshalResponse(value)
}

func (s *Service) engineInfo(ctx context.Context) (EngineSummary, error) {
	reader, ok := s.config.Docker.(DockerInfoReader)
	if !ok {
		return EngineSummary{}, errors.New("agentquery: Docker Engine info reader is unavailable")
	}
	info, err := reader.Info(ctx)
	if err != nil {
		return EngineSummary{}, err
	}
	return EngineSummary{
		Version: info.EngineVersion, CPUCapacity: info.CPUCapacity, MemoryCapacity: info.MemoryCapacity,
		ContainersTotal: info.ContainersTotal, ContainersRunning: info.ContainersRunning, Images: info.Images,
		StorageDriver: info.StorageDriver, LoggingDriver: info.LoggingDriver, CgroupDriver: info.CgroupDriver,
		CgroupVersion: info.CgroupVersion, DefaultRuntime: info.DefaultRuntime, OperatingSystem: info.OperatingSystem,
		OSVersion: info.OSVersion, OSType: info.OSType, Architecture: info.Architecture, KernelVersion: info.KernelVersion,
		DockerRootDir: info.DockerRootDir,
	}, nil
}

func validateEnvelope(request producttransport.QueryRequest) error {
	if request.Kind == "" || len(request.Kind) > 64 || !utf8.ValidString(request.Kind) ||
		len(request.Target) > 1024 || !utf8.ValidString(request.Target) || len(request.Payload) > maxRequestPayloadBytes {
		return ErrInvalidRequest
	}
	return nil
}

func requireEmpty(request producttransport.QueryRequest) error {
	if request.Target != "" || len(request.Payload) != 0 {
		return fmt.Errorf("%w: %s accepts neither target nor payload", ErrInvalidRequest, request.Kind)
	}
	return nil
}

func requireTargetOnly(request producttransport.QueryRequest) error {
	if request.Target == "" || len(request.Payload) != 0 {
		return fmt.Errorf("%w: %s requires only a target", ErrInvalidRequest, request.Kind)
	}
	return nil
}

func requireProjectTarget(request producttransport.QueryRequest) error {
	if !canonicalID.MatchString(request.Target) {
		return fmt.Errorf("%w: project target must be a canonical project UID", ErrInvalidRequest)
	}
	return nil
}

func (s *Service) containerList(ctx context.Context) ([]Container, error) {
	containers, err := s.config.Docker.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(containers) > maxInventoryItems {
		return nil, ErrResponseTooLarge
	}
	result := make([]Container, len(containers))
	for index := range containers {
		result[index] = containerResponse(containers[index])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Service) inventory() (DockerInventoryReader, error) {
	inventory, ok := s.config.Docker.(DockerInventoryReader)
	if !ok {
		return nil, errors.New("agentquery: Docker inventory reader is unavailable")
	}
	return inventory, nil
}

func (s *Service) imageList(ctx context.Context) ([]Image, error) {
	inventory, err := s.inventory()
	if err != nil {
		return nil, err
	}
	items, err := inventory.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) > maxInventoryItems {
		return nil, ErrResponseTooLarge
	}
	result := make([]Image, len(items))
	for index, item := range items {
		if !validDockerObjectID(item.ID) || item.Created < 0 || item.Size < 0 || item.Containers < -1 ||
			!validStringList(item.RepoTags, maxImageReferences, 4096) || !validStringList(item.RepoDigests, maxImageReferences, 4096) {
			return nil, errors.New("agentquery: Docker image reader returned invalid data")
		}
		result[index] = Image{
			ID: item.ID, RepoTags: append([]string(nil), item.RepoTags...), RepoDigests: append([]string(nil), item.RepoDigests...),
			Created: item.Created, Size: item.Size, Containers: item.Containers,
		}
		sort.Strings(result[index].RepoTags)
		sort.Strings(result[index].RepoDigests)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Service) networkList(ctx context.Context) ([]Network, error) {
	inventory, err := s.inventory()
	if err != nil {
		return nil, err
	}
	items, err := inventory.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) > maxInventoryItems {
		return nil, ErrResponseTooLarge
	}
	result := make([]Network, len(items))
	for index, item := range items {
		if !validDockerObjectID(item.ID) || !validBoundedString(item.Name, 255) || !validBoundedString(item.Driver, 128) ||
			!validBoundedString(item.Scope, 64) {
			return nil, errors.New("agentquery: Docker network reader returned invalid data")
		}
		result[index] = Network(item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Service) volumeList(ctx context.Context) ([]Volume, error) {
	inventory, err := s.inventory()
	if err != nil {
		return nil, err
	}
	items, err := inventory.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) > maxInventoryItems {
		return nil, ErrResponseTooLarge
	}
	result := make([]Volume, len(items))
	for index, item := range items {
		if !validBoundedString(item.Name, 255) || !validBoundedString(item.Driver, 128) || !validBoundedString(item.Scope, 64) ||
			len(item.CreatedAt) > 128 || !utf8.ValidString(item.CreatedAt) {
			return nil, errors.New("agentquery: Docker volume reader returned invalid data")
		}
		result[index] = Volume(item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Service) details() (DockerDetailReader, error) {
	reader, ok := s.config.Docker.(DockerDetailReader)
	if !ok {
		return nil, errors.New("agentquery: Docker detail reader is unavailable")
	}
	return reader, nil
}

func (s *Service) imageInspect(ctx context.Context, id string) (ImageDetails, error) {
	if !validDockerObjectID(id) {
		return ImageDetails{}, fmt.Errorf("%w: invalid Image ID", ErrInvalidRequest)
	}
	reader, err := s.details()
	if err != nil {
		return ImageDetails{}, err
	}
	details, err := reader.InspectImage(ctx, id)
	if err != nil {
		return ImageDetails{}, err
	}
	containers, err := s.config.Docker.List(ctx)
	if err != nil {
		return ImageDetails{}, err
	}
	if len(containers) > maxInventoryItems {
		return ImageDetails{}, ErrResponseTooLarge
	}
	result := ImageDetails{ID: details.ID, RepoTags: append([]string(nil), details.RepoTags...), RepoDigests: append([]string(nil), details.RepoDigests...),
		Created: details.Created, Author: details.Author, Architecture: details.Architecture, Variant: details.Variant, OS: details.OS,
		OSVersion: details.OSVersion, Size: details.Size, Entrypoint: append([]string(nil), details.Entrypoint...), Command: append([]string(nil), details.Command...),
		ExposedPorts: append([]string(nil), details.ExposedPorts...), WorkingDir: details.WorkingDir, User: details.User,
		Labels: cloneStringMap(details.Labels), LayerCount: details.LayerCount, UsedBy: []ObjectReference{}}
	for _, container := range containers {
		if strings.TrimPrefix(container.ImageID, "sha256:") != strings.TrimPrefix(details.ID, "sha256:") {
			continue
		}
		result.UsedBy = append(result.UsedBy, containerReference(container, ""))
	}
	sort.Strings(result.RepoTags)
	sort.Strings(result.RepoDigests)
	sort.Strings(result.ExposedPorts)
	sort.Slice(result.UsedBy, func(i, j int) bool { return result.UsedBy[i].ContainerID < result.UsedBy[j].ContainerID })
	return result, nil
}

func (s *Service) networkInspect(ctx context.Context, id string) (NetworkDetails, error) {
	if !canonicalID.MatchString(id) {
		return NetworkDetails{}, fmt.Errorf("%w: invalid Network ID", ErrInvalidRequest)
	}
	reader, err := s.details()
	if err != nil {
		return NetworkDetails{}, err
	}
	details, err := reader.InspectNetwork(ctx, id)
	if err != nil {
		return NetworkDetails{}, err
	}
	containers, err := s.config.Docker.List(ctx)
	if err != nil {
		return NetworkDetails{}, err
	}
	if len(containers) > maxInventoryItems || len(details.Containers) > maxInventoryItems || len(details.IPAM) > 256 {
		return NetworkDetails{}, ErrResponseTooLarge
	}
	byID := make(map[string]dockeradapter.Container, len(containers))
	for _, container := range containers {
		byID[container.ID] = container
	}
	result := NetworkDetails{ID: details.ID, Name: details.Name, Created: details.Created, Scope: details.Scope, Driver: details.Driver,
		EnableIPv4: details.EnableIPv4, EnableIPv6: details.EnableIPv6, Internal: details.Internal, Attachable: details.Attachable,
		Ingress: details.Ingress, ConfigOnly: details.ConfigOnly, IPAMDriver: details.IPAMDriver, Options: cloneStringMap(details.Options), Labels: cloneStringMap(details.Labels),
		ComposeProject: details.Labels["com.docker.compose.project"], ComposeNetwork: details.Labels["com.docker.compose.network"], IPAM: make([]IPAMConfig, len(details.IPAM)), Attachments: []NetworkAttachment{}}
	for index, item := range details.IPAM {
		result.IPAM[index] = IPAMConfig{Subnet: item.Subnet, IPRange: item.IPRange, Gateway: item.Gateway, AuxAddresses: cloneStringMap(item.AuxAddresses)}
	}
	for _, attachment := range details.Containers {
		container := byID[attachment.ContainerID]
		reference := containerReference(container, "")
		reference.ContainerID = attachment.ContainerID
		if reference.ContainerName == "" {
			reference.ContainerName = attachment.Name
		}
		result.Attachments = append(result.Attachments, NetworkAttachment{ObjectReference: reference, EndpointID: attachment.EndpointID, IPv4: attachment.IPv4, IPv6: attachment.IPv6, MAC: attachment.MAC})
	}
	sort.Slice(result.Attachments, func(i, j int) bool { return result.Attachments[i].ContainerID < result.Attachments[j].ContainerID })
	return result, nil
}

func (s *Service) volumeInspect(ctx context.Context, name string) (VolumeDetails, error) {
	if !volumeName.MatchString(name) {
		return VolumeDetails{}, fmt.Errorf("%w: invalid Volume name", ErrInvalidRequest)
	}
	reader, err := s.details()
	if err != nil {
		return VolumeDetails{}, err
	}
	details, err := reader.InspectVolume(ctx, name)
	if err != nil {
		return VolumeDetails{}, err
	}
	containers, err := s.config.Docker.List(ctx)
	if err != nil {
		return VolumeDetails{}, err
	}
	if len(containers) > maxInventoryItems {
		return VolumeDetails{}, ErrResponseTooLarge
	}
	result := VolumeDetails{Name: details.Name, Driver: details.Driver, Scope: details.Scope, CreatedAt: details.CreatedAt, Mountpoint: details.Mountpoint, Options: cloneStringMap(details.Options), Labels: cloneStringMap(details.Labels),
		ComposeProject: details.Labels["com.docker.compose.project"], ComposeVolume: details.Labels["com.docker.compose.volume"], References: []ObjectReference{}}
	for _, container := range containers {
		for _, mount := range container.Mounts {
			if mount.Type == "volume" && mount.Source == name {
				result.References = append(result.References, containerReference(container, mount.Destination))
			}
		}
	}
	sort.Slice(result.References, func(i, j int) bool {
		if result.References[i].ContainerID != result.References[j].ContainerID {
			return result.References[i].ContainerID < result.References[j].ContainerID
		}
		return result.References[i].Destination < result.References[j].Destination
	})
	return result, nil
}

func containerReference(container dockeradapter.Container, destination string) ObjectReference {
	name := ""
	if len(container.Names) > 0 {
		name = strings.TrimPrefix(container.Names[0], "/")
	}
	return ObjectReference{ContainerID: container.ID, ContainerName: name, ComposeProject: container.Labels["com.docker.compose.project"], ComposeService: container.Labels["com.docker.compose.service"], State: container.State, Destination: destination}
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func validDockerObjectID(value string) bool {
	return canonicalID.MatchString(strings.TrimPrefix(value, "sha256:"))
}

func validBoundedString(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validStringList(values []string, maxCount, maxBytes int) bool {
	if len(values) > maxCount {
		return false
	}
	for _, value := range values {
		if len(value) > maxBytes || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return true
}

func (s *Service) fileRead(ctx context.Context, request producttransport.QueryRequest) (FileResponse, error) {
	if err := requireProjectTarget(request); err != nil {
		return FileResponse{}, err
	}
	var input FileReadRequest
	if err := decodeStrict(request.Payload, &input); err != nil {
		return FileResponse{}, err
	}
	if input.RelativePath == "" || len(input.RelativePath) > 4096 || !utf8.ValidString(input.RelativePath) {
		return FileResponse{}, fmt.Errorf("%w: invalid relative_path", ErrInvalidRequest)
	}
	file, err := s.config.Files.Read(ctx, request.Target, input.RelativePath)
	if err != nil {
		return FileResponse{}, err
	}
	defer clear(file.Content)
	return FileResponse{
		RelativePath: file.RelativePath, Content: string(file.Content), SHA256: file.SHA256,
		MTime: file.MTime, Mode: uint32(file.Mode.Perm()), LineEndings: file.LineEndings,
		Secret: isEnvironmentFile(file.RelativePath),
	}, nil
}

func (s *Service) projectEnvironment(ctx context.Context, request producttransport.QueryRequest) ([]EnvironmentEntry, error) {
	if err := requireTargetOnly(request); err != nil {
		return nil, err
	}
	if err := requireProjectTarget(request); err != nil {
		return nil, err
	}
	file, err := s.config.Files.Read(ctx, request.Target, ".env")
	if err != nil {
		return nil, err
	}
	defer clear(file.Content)
	return parseEnvironment(file.Content)
}

func (s *Service) backupList(ctx context.Context, request producttransport.QueryRequest) ([]BackupMetadata, error) {
	if err := requireTargetOnly(request); err != nil {
		return nil, err
	}
	if err := requireProjectTarget(request); err != nil {
		return nil, err
	}
	metadata, err := s.config.Backups.List(ctx, request.Target)
	if err != nil {
		return nil, err
	}
	result := make([]BackupMetadata, len(metadata))
	for index, item := range metadata {
		if item.ProjectUID != request.Target || !safeBackupID.MatchString(item.BackupID) || item.BackupID == "." || item.BackupID == ".." ||
			item.CreatedAt.IsZero() || item.FileCount < 0 || item.SizeBytes < 0 || !canonicalID.MatchString(item.ManifestSHA256) ||
			!validBackupTrigger(item.Trigger) {
			return nil, errors.New("agentquery: backup reader returned invalid metadata")
		}
		manifest, manifestErr := s.config.Backups.LoadManifest(item.ProjectUID, item.BackupID)
		if manifestErr != nil || len(manifest.Files) != item.FileCount || len(manifest.Files) > 4096 {
			return nil, errors.New("agentquery: backup manifest metadata is unavailable or inconsistent")
		}
		paths := make([]string, len(manifest.Files))
		for fileIndex, file := range manifest.Files {
			if !validRelativeManifestPath(file.RelPath) {
				return nil, errors.New("agentquery: backup manifest returned an invalid path")
			}
			paths[fileIndex] = filepath.ToSlash(file.RelPath)
		}
		sort.Strings(paths)
		result[index] = BackupMetadata{
			BackupID: item.BackupID, ProjectUID: item.ProjectUID, CreatedAt: item.CreatedAt.UTC(),
			Trigger: item.Trigger, FileCount: item.FileCount, SizeBytes: item.SizeBytes,
			ManifestSHA256: item.ManifestSHA256, Paths: paths,
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].BackupID < result[j].BackupID
	})
	return result, nil
}

func validRelativeManifestPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || filepath.IsAbs(value) {
		return false
	}
	cleaned := filepath.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

func (s *Service) composePS(ctx context.Context, request producttransport.QueryRequest) (ComposeOutput, error) {
	if err := requireProjectTarget(request); err != nil {
		return ComposeOutput{}, err
	}
	var input ComposePSRequest
	if err := decodeStrict(request.Payload, &input); err != nil {
		return ComposeOutput{}, err
	}
	if err := validateComposeServices(input.Services); err != nil {
		return ComposeOutput{}, err
	}
	return s.runComposeQuery(ctx, request.Target, composeexec.Spec{
		Operation: composeexec.OperationPS,
		Services:  append([]string(nil), input.Services...),
		Flags:     composeexec.Flags{PSAll: input.All},
	})
}

func (s *Service) composeConfig(ctx context.Context, request producttransport.QueryRequest) (ComposeOutput, error) {
	if err := requireProjectTarget(request); err != nil {
		return ComposeOutput{}, err
	}
	var input ComposeConfigRequest
	if err := decodeStrict(request.Payload, &input); err != nil {
		return ComposeOutput{}, err
	}
	if err := validateComposeServices(input.Services); err != nil {
		return ComposeOutput{}, err
	}
	return s.runComposeQuery(ctx, request.Target, composeexec.Spec{
		Operation: composeexec.OperationConfig,
		Services:  append([]string(nil), input.Services...),
		Flags:     composeexec.Flags{ConfigNoInterpolate: !input.Reveal},
	})
}

func validateComposeServices(services []string) error {
	if len(services) > 256 {
		return fmt.Errorf("%w: too many Compose services", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(services))
	for _, service := range services {
		if len(service) > 128 || !serviceName.MatchString(service) {
			return fmt.Errorf("%w: invalid Compose service", ErrInvalidRequest)
		}
		if _, duplicate := seen[service]; duplicate {
			return fmt.Errorf("%w: duplicate Compose service", ErrInvalidRequest)
		}
		seen[service] = struct{}{}
	}
	return nil
}

func (s *Service) runComposeQuery(ctx context.Context, projectUID string, spec composeexec.Spec) (ComposeOutput, error) {
	project, found, err := s.config.Projects.Project(ctx, projectUID)
	if err != nil {
		return ComposeOutput{}, err
	}
	if !found {
		return ComposeOutput{}, ErrProjectUnavailable
	}
	spec.Project = project
	spec.OutputTailBytes = maxComposeQueryBytes
	result, err := s.config.Compose.Run(ctx, spec, nil)
	defer clear(result.Tail)
	if err != nil {
		return ComposeOutput{}, fmt.Errorf("agentquery: execute %s: %w", spec.Operation, err)
	}
	if result.TailTruncated {
		return ComposeOutput{}, ErrResponseTooLarge
	}
	if !result.Success() {
		return ComposeOutput{}, fmt.Errorf("agentquery: docker compose %s exited with status %d", spec.Operation, result.ExitCode)
	}
	if !utf8.Valid(result.Tail) {
		return ComposeOutput{}, errors.New("agentquery: Docker Compose output is not valid UTF-8")
	}
	return ComposeOutput{Output: string(result.Tail)}, nil
}

func containerResponse(value dockeradapter.Container) Container {
	mounts := make([]Mount, len(value.Mounts))
	for index, mount := range value.Mounts {
		mounts[index] = Mount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, ReadWrite: mount.ReadWrite}
	}
	labels := make(map[string]string, len(value.Labels))
	for key, labelValue := range value.Labels {
		labels[key] = labelValue
	}
	if value.Labels == nil {
		labels = nil
	}
	result := Container{
		ID: value.ID, Names: append([]string(nil), value.Names...), Image: value.Image, ImageID: value.ImageID,
		State: value.State, Status: value.Status, Labels: labels, Mounts: mounts,
		Health: value.Health, ComposeProject: value.Labels["com.docker.compose.project"],
		ComposeService: value.Labels["com.docker.compose.service"],
		OneOff:         strings.EqualFold(value.Labels["com.docker.compose.oneoff"], "true"),
		Ports:          make([]PublishedPort, len(value.Ports)), Protected: value.Protected, ProtectionReason: value.ProtectionReason,
		ExitCode: value.ExitCode, CreatedAt: value.CreatedAt, StartedAt: value.StartedAt, FinishedAt: value.FinishedAt,
		OOMKilled: value.OOMKilled, RestartCount: value.RestartCount, RestartPolicy: value.RestartPolicy,
		RestartMaximumRetry: value.RestartMaximumRetry, StopSignal: value.StopSignal, StopTimeout: value.StopTimeout,
		LoggingDriver: value.LoggingDriver, Command: append([]string(nil), value.Command...),
		Entrypoint: append([]string(nil), value.Entrypoint...), ExposedPorts: append([]string(nil), value.ExposedPorts...),
		Networks: make([]ContainerNetwork, len(value.Networks)),
	}
	for index, port := range value.Ports {
		result.Ports[index] = PublishedPort{HostIP: port.HostIP, PublishedPort: port.PublishedPort, TargetPort: port.TargetPort, Protocol: port.Protocol}
	}
	for index, network := range value.Networks {
		result.Networks[index] = ContainerNetwork{Name: network.Name, NetworkID: network.NetworkID, EndpointID: network.EndpointID,
			IPv4: network.IPv4, IPv6: network.IPv6, MAC: network.MAC, Aliases: append([]string(nil), network.Aliases...)}
		sort.Strings(result.Networks[index].Aliases)
	}
	sort.Strings(result.ExposedPorts)
	sort.Slice(result.Networks, func(i, j int) bool { return result.Networks[i].Name < result.Networks[j].Name })
	sort.Strings(result.Names)
	sort.Slice(result.Mounts, func(i, j int) bool {
		if result.Mounts[i].Destination != result.Mounts[j].Destination {
			return result.Mounts[i].Destination < result.Mounts[j].Destination
		}
		return result.Mounts[i].Source < result.Mounts[j].Source
	})
	sort.Slice(result.Ports, func(i, j int) bool {
		if result.Ports[i].TargetPort != result.Ports[j].TargetPort {
			return result.Ports[i].TargetPort < result.Ports[j].TargetPort
		}
		if result.Ports[i].PublishedPort != result.Ports[j].PublishedPort {
			return result.Ports[i].PublishedPort < result.Ports[j].PublishedPort
		}
		if result.Ports[i].Protocol != result.Ports[j].Protocol {
			return result.Ports[i].Protocol < result.Ports[j].Protocol
		}
		return result.Ports[i].HostIP < result.Ports[j].HostIP
	})
	return result
}

func projectListResponse(projects []agentprojects.Project, status agentprojects.ScanStatus, containers []dockeradapter.Container, recoveryBlocked func(string) bool) (ProjectListResponse, error) {
	dockerFacts, err := composeDockerFacts(containers)
	if err != nil {
		return ProjectListResponse{}, err
	}
	result := ProjectListResponse{Projects: make([]Project, len(projects)), DockerFacts: dockerFacts, Status: scanStatusResponse(status)}
	for index, project := range projects {
		if len(project.SourceReferences) > maxProjectSourceRefs {
			return ProjectListResponse{}, ErrResponseTooLarge
		}
		result.Projects[index] = projectResponse(project, recoveryBlocked(project.UID))
	}
	sort.Slice(result.Projects, func(i, j int) bool { return result.Projects[i].UID < result.Projects[j].UID })
	return result, nil
}

func composeDockerFacts(containers []dockeradapter.Container) ([]DockerProjectFact, error) {
	result := make([]DockerProjectFact, 0)
	for _, container := range containers {
		projectName := container.Labels["com.docker.compose.project"]
		if projectName == "" {
			continue
		}
		if len(result) == maxProjectDockerFacts {
			return nil, ErrResponseTooLarge
		}
		fact := DockerProjectFact{
			ContainerID: container.ID,
			ProjectName: projectName,
			WorkingDir:  container.Labels["com.docker.compose.project.working_dir"],
			Service:     container.Labels["com.docker.compose.service"],
			ConfigHash:  container.Labels["com.docker.compose.config-hash"],
		}
		if rawFiles := container.Labels["com.docker.compose.project.config_files"]; rawFiles != "" {
			fact.ConfigFiles = strings.Split(rawFiles, ",")
		}
		result = append(result, fact)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

func projectResponse(project agentprojects.Project, recoveryBlocked bool) Project {
	files := make([]FileFact, len(project.Files))
	for index, file := range project.Files {
		files[index] = FileFact{Path: file.Path, Size: file.Size, SHA256: file.SHA256}
	}
	result := Project{
		UID: project.UID, Root: project.Root, WorkingDir: project.WorkingDir, Files: files,
		Name: project.Name, Services: append([]string(nil), project.Services...),
		ActiveProfiles:   append([]string(nil), project.ActiveProfiles...),
		IncludedWorkDirs: append([]string(nil), project.IncludedWorkDirs...), SourceGraphComplete: project.SourceGraphComplete,
		CurrentFingerprint: project.CurrentFingerprint, ComposeExecutable: project.ComposeExecutable,
		FilesystemWritable: project.FilesystemWritable, CapabilityReason: project.CapabilityReason, Stale: project.Stale,
		RestoreRecoveryRequired: recoveryBlocked,
	}
	result.ComposeFiles = make([]string, 0, len(project.ComposeFiles))
	for _, path := range project.ComposeFiles {
		relative, err := filepath.Rel(project.WorkingDir, path)
		if err == nil && relative != "." && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			result.ComposeFiles = append(result.ComposeFiles, filepath.ToSlash(relative))
		}
	}
	policy, policyErr := composeexec.EvaluateV1Policy(project.ServiceModels)
	decisionByName := make(map[string]composeexec.ServiceDecision, len(policy.Services))
	for _, decision := range policy.Services {
		decisionByName[decision.Name] = decision
	}
	result.DefinedServices = make([]ServiceModel, 0, len(project.ServiceModels))
	for _, model := range project.ServiceModels {
		decision := decisionByName[model.Name]
		result.DefinedServices = append(result.DefinedServices, ServiceModel{
			Name: model.Name, Image: model.Image, HasBuild: model.HasBuild, PullPolicy: model.PullPolicy,
			Profiles: append([]string(nil), model.Profiles...), DependsOn: append([]string(nil), model.DependsOn...),
			Active: model.Active, BuildRequired: model.BuildRequired(), PullAvailable: decision.PullAvailable,
			UpAvailable: decision.UpAvailable, UnavailableReason: decision.UnavailableReason,
		})
	}
	if policyErr == nil {
		result.PullServices = append([]string(nil), policy.PullServices...)
		result.ProjectUpAvailable = policy.ProjectUpAvailable
		result.ProjectUpReason = policy.ProjectUpReason
	} else {
		result.ProjectUpReason = "effective Compose Service policy is unavailable"
	}
	result.EnvFiles = make([]EnvFileReference, len(project.EnvFiles))
	for index, reference := range project.EnvFiles {
		result.EnvFiles[index] = EnvFileReference{Path: displayProjectPath(project.WorkingDir, reference.Path), Readable: reference.Readable}
	}
	result.Secrets = make([]ResourceSource, len(project.Secrets))
	for index, source := range project.Secrets {
		result.Secrets[index] = ResourceSource(source)
	}
	result.Configs = make([]ResourceSource, len(project.Configs))
	for index, source := range project.Configs {
		result.Configs[index] = ResourceSource(source)
	}
	result.SourceReferences = make([]SourceReference, len(project.SourceReferences))
	for index, reference := range project.SourceReferences {
		result.SourceReferences[index] = SourceReference{
			Kind: reference.Kind, Path: reference.Path, Accessible: reference.Accessible, ReadOnly: reference.ReadOnly,
		}
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	sort.Strings(result.Services)
	sort.Strings(result.ActiveProfiles)
	sort.Slice(result.DefinedServices, func(i, j int) bool { return result.DefinedServices[i].Name < result.DefinedServices[j].Name })
	sort.Strings(result.IncludedWorkDirs)
	sort.Slice(result.SourceReferences, func(left, right int) bool {
		if result.SourceReferences[left].Kind != result.SourceReferences[right].Kind {
			return result.SourceReferences[left].Kind < result.SourceReferences[right].Kind
		}
		return result.SourceReferences[left].Path < result.SourceReferences[right].Path
	})
	return result
}

func displayProjectPath(workingDir, path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path)
	}
	relative, err := filepath.Rel(workingDir, path)
	if err == nil && relative != "." && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(relative)
	}
	return filepath.ToSlash(path)
}

func scanStatusResponse(status agentprojects.ScanStatus) ScanStatus {
	return ScanStatus{
		ScannedAt: status.ScannedAt.UTC(), Truncated: status.Truncated, StopReason: string(status.StopReason),
		DirectoriesSeen: status.DirectoriesSeen, LastScannedPath: status.LastScannedPath,
	}
}

func decodeStrict(payload []byte, output any) error {
	if len(payload) == 0 {
		return fmt.Errorf("%w: JSON payload is required", ErrInvalidRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("%w: invalid JSON payload: %v", ErrInvalidRequest, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: payload must contain exactly one JSON value", ErrInvalidRequest)
	}
	return nil
}

func marshalResponse(value any) (producttransport.QueryResponse, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return producttransport.QueryResponse{}, fmt.Errorf("agentquery: encode response: %w", err)
	}
	if len(payload) > maxResponsePayloadBytes {
		clear(payload)
		return producttransport.QueryResponse{}, ErrResponseTooLarge
	}
	return producttransport.QueryResponse{Payload: payload}, nil
}

func validBackupTrigger(trigger backup.Trigger) bool {
	return trigger == backup.TriggerManual || trigger == backup.TriggerPreWrite || trigger == backup.TriggerPreRestore
}

func isEnvironmentFile(path string) bool {
	return path == ".env" || strings.HasPrefix(path, ".env.")
}

func parseEnvironment(content []byte) ([]EnvironmentEntry, error) {
	lines := strings.Split(string(content), "\n")
	entries := make([]EnvironmentEntry, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for lineNumber, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		}
		name, rawValue, ok := strings.Cut(trimmed, "=")
		name = strings.TrimSpace(name)
		if !ok || !envName.MatchString(name) {
			return nil, fmt.Errorf("agentquery: invalid .env assignment on line %d", lineNumber+1)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("agentquery: duplicate .env variable %q", name)
		}
		value, err := decodeEnvironmentValue(strings.TrimSpace(rawValue))
		if err != nil {
			return nil, fmt.Errorf("agentquery: invalid .env value for %q on line %d: %w", name, lineNumber+1, err)
		}
		seen[name] = struct{}{}
		entries = append(entries, EnvironmentEntry{Name: name, Value: value, Secret: true})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func decodeEnvironmentValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unterminated single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] == '"' {
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", errors.New("unterminated double-quoted value")
		}
		var decoded string
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return "", err
		}
		return decoded, nil
	}
	if comment := strings.Index(value, " #"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	return value, nil
}
