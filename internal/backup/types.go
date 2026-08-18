package backup

import (
	"context"
	"errors"
	"time"
)

const AutomaticSnapshotRetention = 20

type Trigger string

const (
	TriggerManual     Trigger = "manual"
	TriggerPreWrite   Trigger = "pre_write"
	TriggerPreRestore Trigger = "pre_restore"
)

var (
	ErrInvalidPath            = errors.New("invalid backup path")
	ErrSymlink                = errors.New("backup source or target is a symlink")
	ErrConcurrentModification = errors.New("backup source changed while being read")
	ErrInvalidArchive         = errors.New("invalid backup archive")
	ErrCommitCanceled         = errors.New("restore canceled before commit")
	ErrRecoveryRequired       = errors.New("restore recovery required")
	ErrProjectRecoveryBlocked = errors.New("project changes blocked by restore recovery")
)

type Project struct {
	UID        string
	Name       string
	WorkingDir string
}

type FileEntry struct {
	RelPath string `json:"rel_path"`
	SHA256  string `json:"sha256"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
}

type Manifest struct {
	BackupID    string      `json:"backup_id"`
	ProjectUID  string      `json:"project_uid"`
	ProjectName string      `json:"project_name"`
	WorkingDir  string      `json:"working_dir"`
	CreatedAt   time.Time   `json:"created_at"`
	Trigger     Trigger     `json:"trigger"`
	OperationID string      `json:"operation_id"`
	Files       []FileEntry `json:"files"`
}

type Metadata struct {
	BackupID       string
	ProjectUID     string
	CreatedAt      time.Time
	Trigger        Trigger
	FileCount      int
	SizeBytes      int64
	ManifestSHA256 string
}

// MetadataSink is the Server index boundary. No file content or archive path is
// exposed through this interface.
type MetadataSink interface {
	RecordBackup(context.Context, Metadata) error
}

type Admission struct {
	ProjectUID     string
	Trigger        Trigger
	EstimatedBytes int64
}

// BudgetAdmitter is implemented by the separate disk-budget layer. This
// package never invents eviction order or degraded-storage policy.
type BudgetAdmitter interface {
	AdmitBackup(context.Context, Admission) error
	AdmitRestore(context.Context, RestoreAdmission) error
}

type RestoreAdmission struct {
	ProjectUID           string
	BackupID             string
	FilesystemTotalBytes int64
	FilesystemFreeBytes  int64
	EstimatedBytes       int64
}

type CreateRequest struct {
	Project       Project
	RelativePaths []string
	Trigger       Trigger
	OperationID   string
	CreatedAt     time.Time
}

type Backup struct {
	Manifest Manifest
	Metadata Metadata
}

type CommitGate interface {
	EnterRestoreCommit(context.Context) error
}

type CommitGateFunc func(context.Context) error

func (function CommitGateFunc) EnterRestoreCommit(ctx context.Context) error { return function(ctx) }

type RestoreRequest struct {
	Project     Project
	BackupID    string
	OperationID string
	Now         time.Time
	CommitGate  CommitGate
	// Progress is called after each durable target replacement. Returning an
	// error enters the existing restore rollback/recovery path.
	Progress func(completed, total int) error
}

type RestoreResult struct {
	PreRestoreSnapshotID string
	RestoredFiles        int
	RolledBack           bool
}

type RecoveryResult struct {
	OperationID      string
	ProjectUID       string
	Interrupted      bool
	RolledBack       bool
	RecoveryRequired bool
	Err              error
}

type ProjectResolver interface {
	ResolveBackupProject(context.Context, string) (Project, error)
}

type ProjectResolverFunc func(context.Context, string) (Project, error)

func (function ProjectResolverFunc) ResolveBackupProject(ctx context.Context, uid string) (Project, error) {
	return function(ctx, uid)
}
