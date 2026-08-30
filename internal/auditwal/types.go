package auditwal

import (
	"time"

	productconfig "github.com/east-true/docklattice/internal/config"
)

const (
	DefaultMaxBytes     int64 = 256 << 20
	DefaultMaxAge             = 14 * 24 * time.Hour
	DefaultSyncInterval       = time.Second
	DefaultSyncBytes    int64 = 64 << 10
)

type Options struct {
	MaxBytes     int64
	MaxAge       time.Duration
	SyncInterval time.Duration
	SyncBytes    int64
	Now          func() time.Time
}

func DefaultOptions() Options {
	defaults := productconfig.V1Defaults()
	return Options{
		MaxBytes: defaults.WALMaxBytes, MaxAge: defaults.WALRetention,
		SyncInterval: defaults.WALFsyncInterval, SyncBytes: defaults.WALFsyncBytes,
		Now: time.Now,
	}
}

type Cursor struct {
	Incarnation uint64 `json:"incarnation"`
	Seq         uint64 `json:"seq"`
}

type Record struct {
	AgentID    string
	Cursor     Cursor
	AppendedAt time.Time
	Payload    []byte
}

type GapReason string
type Precision string

const (
	GapRetention       GapReason = "RETENTION"
	GapDiskPressure    GapReason = "DISK_PRESSURE"
	PrecisionExact     Precision = "exact"
	PrecisionCoalesced Precision = "coalesced"
)

type Gap struct {
	Incarnation      uint64    `json:"incarnation"`
	FromSeq          uint64    `json:"from_seq"`
	UntilSeq         uint64    `json:"until_seq"`
	Reason           GapReason `json:"reason"`
	Precision        Precision `json:"precision"`
	LastLossRevision uint64    `json:"last_loss_revision"`
}

type CoverageSnapshot struct {
	AgentID                     string
	Revision                    uint64
	GeneratedAt                 time.Time
	Gaps                        []Gap
	CoverageUnknownIncarnations []uint64
}

type Bounds struct {
	WALFloor              *Cursor
	WALCeiling            *Cursor
	NextCursor            Cursor
	DurableThrough        *Cursor
	ServerACKedThrough    *Cursor
	AcknowledgedArchiveID string
	CoverageRevision      uint64
}

type CursorBehindFloor struct {
	Requested Cursor
	Bounds    Bounds
	Coverage  CoverageSnapshot
}

type ReadResult struct {
	Records     []Record
	BehindFloor *CursorBehindFloor
}

type ReclaimResult struct {
	FreedBytes          int64
	DeletedACKedBytes   int64
	DeletedUnackedBytes int64
	Degraded            bool
	Coverage            CoverageSnapshot
}

// Recovery is the pre-startup WAL fact used by Agent state to assess the
// previous clean_close before it increments the incarnation.
type Recovery struct {
	WALTail        *Cursor
	DurableThrough *Cursor
}
