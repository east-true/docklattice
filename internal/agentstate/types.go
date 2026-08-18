package agentstate

import "time"

const (
	StateFileName = "agent-state.json"
	stateVersion  = 1
)

// Cursor is an Audit cursor. It is never flattened because subtraction is not
// defined across incarnation boundaries.
type Cursor struct {
	Incarnation uint64 `json:"incarnation"`
	Seq         uint64 `json:"seq"`
}

// Credential stores either an external credential file reference or the
// signed credential bytes themselves. Both forms contain credential material,
// never a join token.
type Credential struct {
	FileReference string `json:"file_reference,omitempty"`
	Data          []byte `json:"data,omitempty"`
}

// PendingCredentialActivation makes Renew -> durable save -> Activate
// restart-safe. Previous is retained only until the Server confirms activation
// (revocation of the previous credential).
type PendingCredentialActivation struct {
	Previous           Credential `json:"previous"`
	ActiveCredentialID string     `json:"active_credential_id"`
}

// ArchiveBinding is the currently acknowledged Server archive identity.
type ArchiveBinding struct {
	ServerIdentityID string  `json:"server_identity_id"`
	Generation       uint64  `json:"generation"`
	ArchiveID        string  `json:"archive_id"`
	CoverageBeginsAt Cursor  `json:"coverage_begins_at"`
	AckedThrough     *Cursor `json:"acked_through,omitempty"`
}

// RetiredArchive preserves the prior generation binding during a forward
// Archive Rebind. Its ACK is evidence about the retired archive only.
type RetiredArchive struct {
	Generation   uint64  `json:"generation"`
	ArchiveID    string  `json:"archive_id"`
	AckedThrough *Cursor `json:"acked_through,omitempty"`
	RetiredAt    string  `json:"retired_at"`
}

// CleanClose proves that the named incarnation was closed after its WAL was
// durably flushed through LastDurableSeq.
type CleanClose struct {
	Incarnation    uint64 `json:"incarnation"`
	LastDurableSeq uint64 `json:"last_durable_seq"`
	ClosedAt       string `json:"closed_at"`
}

// Snapshot is a defensive copy of the durable Agent state.
type Snapshot struct {
	AgentID              string
	Credential           Credential
	PendingActivation    *PendingCredentialActivation
	BoundArchive         *ArchiveBinding
	RetiredArchives      []RetiredArchive
	CurrentIncarnation   uint64
	CleanClose           *CleanClose
	LastDockerEventAt    time.Time
	DockerSnapshotSHA256 string
}

// Inspection is the read-only identity fact needed to recover the WAL before
// Open durably advances the incarnation. Exists=false means neither a state
// file nor an Agent identity has been created yet.
type Inspection struct {
	Exists             bool
	AgentID            string
	CurrentIncarnation uint64
}

// Startup describes the previous incarnation assessment. Open returns only
// after the incremented incarnation has passed file and directory fsync.
type Startup struct {
	AgentID             string
	PreviousIncarnation uint64
	CurrentIncarnation  uint64
	PreviousUnclean     bool
	KnownDurableThrough *Cursor
}

// RebindResult gives the caller enough information to emit ARCHIVE_REBOUND to
// its WAL. A repeated request for the active tuple returns Changed=false.
type RebindResult struct {
	Changed  bool
	Previous *RetiredArchive
	Current  ArchiveBinding
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
