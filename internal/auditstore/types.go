package auditstore

import (
	"context"
	"encoding/json"
	"time"
)

type Cursor struct {
	Incarnation uint64
	Seq         uint64
}

type Event struct {
	AgentID      string
	Cursor       Cursor
	OccurredAt   time.Time
	Kind         string
	Actor        string
	ProjectUID   string
	OperationID  string
	ResourceType string
	ResourceID   string
	Action       string
	Metadata     json.RawMessage
}

type Precision string

const (
	PrecisionExact     Precision = "exact"
	PrecisionCoalesced Precision = "coalesced"
	PrecisionUnknown   Precision = "unknown"
)

type GapClaim struct {
	Incarnation uint64
	FromSeq     uint64
	UntilSeq    uint64
	Reason      string
	Precision   Precision
}

type CoverageSnapshot struct {
	AgentID                     string
	Revision                    uint64
	GeneratedAt                 time.Time
	Gaps                        []GapClaim
	CoverageUnknownIncarnations []uint64
}

type CoverageStartReason string

const (
	CoverageServerNeverHad        CoverageStartReason = "SERVER_NEVER_HAD"
	CoverageNewAuditArchive       CoverageStartReason = "NEW_AUDIT_ARCHIVE"
	CoverageDatabaseReinitialized CoverageStartReason = "SERVER_DATABASE_REINITIALIZED"
)

type Range struct {
	From  Cursor
	Until Cursor
}

type IngestResult struct {
	Inserted     int
	Duplicates   int
	DeliveryNext Cursor
}

type ClaimResult struct {
	Applied         bool
	CurrentRevision uint64
}

type ACKResult struct {
	Advanced bool
	Cursor   Cursor
}

type EffectiveGap struct {
	ID            int64
	Range         *Range
	Incarnation   uint64
	Precision     Precision
	Source        string
	EstablishedAt time.Time
}

type Observation struct {
	ACKWatermarkStalled         time.Duration
	ACKCursor                   *Cursor
	CoverageRevisionSeen        uint64
	CoverageRevisionCurrent     uint64
	StaleCoverageTotal          uint64
	ACKRetryTotal               uint64
	ACKBlockedWhileIngesting    bool
	ACKBlockedWhileIngestingFor time.Duration
	IngestedUnackedRecords      int64
	IngestedUnackedBytes        int64
	EffectiveGapRecords         int64
	AgentGapClaimsTotal         int64
}

const (
	DefaultServerAuditRetention = 365 * 24 * time.Hour
	DefaultServerAuditMaxBytes  = int64(10) << 30
	DefaultWarningPercent       = 80
	DefaultAggressivePercent    = 95
	DefaultLowWatermarkPercent  = 80
)

// RetentionPolicy is a pure decision boundary. The executor, not the policy,
// owns deletion ordering and transactional Coverage Ledger writes.
type RetentionPolicy interface {
	Plan(context.Context, ArchiveUsage) (RetentionPlan, error)
}

type ArchiveUsage struct {
	AuditArchiveID   string
	RecordCount      int64
	ApproximateBytes int64
	OldestOccurredAt time.Time
	EvaluatedAt      time.Time
}

type RetentionPlan struct {
	DeleteBefore        time.Time
	Warning             bool
	Aggressive          bool
	PressureTargetBytes int64
}

type RetentionLevel string

const (
	RetentionNormal     RetentionLevel = "NORMAL"
	RetentionWarning    RetentionLevel = "WARNING"
	RetentionAggressive RetentionLevel = "AGGRESSIVE"
)

type RetentionResult struct {
	Level                        RetentionLevel
	UsageBefore                  ArchiveUsage
	UsageAfter                   ArchiveUsage
	DeletedACKedRecords          int64
	DeletedUnACKedRecords        int64
	DeletedCoveredRecords        int64
	CompactedCoverageHistoryRows int64
	CreatedCoverageIntervals     int64
	LowWatermarkReached          bool
}
