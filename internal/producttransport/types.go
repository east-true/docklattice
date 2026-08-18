// Package producttransport provides the product Reverse gRPC session boundary.
// Its exported contract is independent of gRPC, HTTP/2, ALPN, and protobuf.
package producttransport

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// CurrentProductProtocolVersion is release N. The Server accepts exactly N
	// and N-1; no wider range is inferred from numeric ordering.
	CurrentProductProtocolVersion uint32 = 2
	// PreviousProductProtocolVersion is the sole supported protocol N-1.
	PreviousProductProtocolVersion uint32 = 1
	// ProtocolVersion and MinCompatibleProtocolVersion remain source-compatible
	// names for callers while pointing at the explicit N/N-1 contract.
	ProtocolVersion              = CurrentProductProtocolVersion
	MinCompatibleProtocolVersion = PreviousProductProtocolVersion
	DefaultMaxCredentialBytes    = 64 << 10
	DefaultMaxMessageBytes       = 1 << 20
	MaxOperationIDBytes          = 256
	MaxCancelReasonBytes         = 32
	// MaxActiveOperationCount matches the bounded v1 operation result index.
	// Active-operation reconciliation fails closed instead of truncating this
	// authoritative snapshot.
	MaxActiveOperationCount     = 500
	MaxOperationOutputTailBytes = 64 << 10
	DefaultOfflineAfter         = 90 * time.Second
	DefaultHeartbeatInterval    = 30 * time.Second
	DefaultHeartbeatTimeout     = 10 * time.Second
	DefaultHandshakeTimeout     = 10 * time.Second
)

func supportedProductProtocolVersion(version uint32) bool {
	return version == CurrentProductProtocolVersion || version == PreviousProductProtocolVersion
}

var (
	ErrAuthentication     = errors.New("agent authentication failed")
	ErrCredentialRevoked  = errors.New("agent credential revoked")
	ErrProtocol           = errors.New("product transport protocol error")
	ErrSessionReplaced    = errors.New("agent session replaced")
	ErrStaleIncarnation   = errors.New("stale agent incarnation")
	ErrHandlerUnavailable = errors.New("agent capability handler unavailable")
)

type SessionID string

type SessionInfo struct {
	SessionID        SessionID
	AgentID          string
	CredentialID     string
	ServerIdentityID string
	Incarnation      uint64
	ProtocolVersion  uint32
}

type CredentialIdentity struct {
	AgentID          string
	CredentialID     string
	ServerIdentityID string
}

// CredentialVerifier must verify signature, server identity, expiry, and the
// durable revocation ledger before returning an identity.
type CredentialVerifier interface {
	VerifyCredential(context.Context, []byte, time.Time) (CredentialIdentity, error)
}

type CredentialVerifierFunc func(context.Context, []byte, time.Time) (CredentialIdentity, error)

func (f CredentialVerifierFunc) VerifyCredential(ctx context.Context, credential []byte, now time.Time) (CredentialIdentity, error) {
	return f(ctx, credential, now)
}

type Capability struct {
	ConnectionReady       bool
	DockerReady           bool
	ComposeReady          bool
	DockerAPIVersion      string
	BundledComposeVersion string
	Reason                string
	// FSRead and FSWrite are owned by the Agent, which performs the per-root
	// identical-path self-check. An Agent one protocol version behind leaves
	// them false with empty reasons, which the Server reports as not reported.
	FSRead        bool
	FSWrite       bool
	FSReadReason  string
	FSWriteReason string
}

type Heartbeat struct {
	SentAt     time.Time
	ObservedAt time.Time
	Capability Capability
}

// AgentHandler is the minimum transport-neutral Agent control surface.
// QueryHandler and OperationHandler are discovered independently so agents may
// expose only the capabilities they actually implement.
type AgentHandler interface {
	Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error)
}

type AgentHandlerFunc func(context.Context, SessionInfo, time.Time) (Capability, error)

func (f AgentHandlerFunc) Heartbeat(ctx context.Context, info SessionInfo, sentAt time.Time) (Capability, error) {
	return f(ctx, info, sentAt)
}

type QueryRequest struct {
	Kind    string
	Target  string
	Payload []byte
}

type QueryResponse struct {
	Payload []byte
}

type QueryHandler interface {
	Query(context.Context, SessionInfo, QueryRequest) (QueryResponse, error)
}

type OperationRequest struct {
	OperationID string
	Type        string
	ProjectKey  string
	Target      string
	Payload     []byte
}

type OperationResponse struct {
	Status                 string
	Phase                  string
	Revision               uint64
	PartialEffectsPossible bool
	Error                  string
	OutputTail             []byte
	OutputTruncated        bool
}

type OperationHandler interface {
	// StartOperation must durably accept or locate the operation and return its
	// accepted/current record without waiting for long-running execution.
	StartOperation(context.Context, SessionInfo, OperationRequest) (OperationResponse, error)
}

type GetOperationRequest struct{ OperationID string }

type GetOperationResponse struct {
	Found     bool
	Operation OperationResponse
}

type CancelOperationRequest struct {
	OperationID string
	Reason      string
}

type CancelOperationResponse struct {
	Outcome   string
	Operation OperationResponse
}

type ListActiveOperationsRequest struct{}

type ActiveOperation struct {
	OperationID string
	Type        string
	ProjectKey  string
	Target      string
	Operation   OperationResponse
}

type ListActiveOperationsResponse struct {
	Operations []ActiveOperation
}

// OperationControlHandler is the Agent-authoritative reconnect and explicit
// cancellation surface. Transport disconnection never invokes either method.
type OperationControlHandler interface {
	GetOperation(context.Context, SessionInfo, GetOperationRequest) (GetOperationResponse, error)
	CancelOperation(context.Context, SessionInfo, CancelOperationRequest) (CancelOperationResponse, error)
}

// OperationRecoveryHandler is separate from OperationControlHandler so N-1
// handlers continue to compile and receive typed-unimplemented behavior.
type OperationRecoveryHandler interface {
	ListActiveOperations(context.Context, SessionInfo, ListActiveOperationsRequest) (ListActiveOperationsResponse, error)
}

type LogRequest struct {
	ContainerID string
	ProjectUID  string
	Services    []string
	Follow      bool
	TailLines   uint64
	ShowStdout  bool
	ShowStderr  bool
	Timestamps  bool
}

type LogEvent struct {
	Data         []byte
	Stream       string
	LineCount    uint64
	Timestamp    time.Time
	DroppedBytes uint64
	DroppedLines uint64
	Terminal     bool
	Error        string
}

type LogSender interface{ Send(LogEvent) error }

type LogStreamHandler interface {
	StreamLogs(context.Context, SessionInfo, LogRequest, LogSender) error
}

type StatsRequest struct{ ContainerID string }

type StatsSample struct {
	ContainerID  string
	ObservedAt   time.Time
	CPUPercent   float64
	MemoryUsage  uint64
	MemoryLimit  uint64
	NetworkRX    uint64
	NetworkTX    uint64
	BlockRead    uint64
	BlockWrite   uint64
	RestartCount uint64
	Health       string
	Uptime       time.Duration
}

type StatsSender interface{ Send(StatsSample) error }

type StatsStreamHandler interface {
	StreamStats(context.Context, SessionInfo, StatsRequest, StatsSender) error
}

type LogReceiveStream interface {
	Recv(context.Context) (LogEvent, error)
	Close() error
}

type StatsReceiveStream interface {
	Recv(context.Context) (StatsSample, error)
	Close() error
}

// AuditSyncHandler is the transport-neutral P1 integration boundary. The
// durable WAL owns sequencing and retention; producttransport intentionally
// does not import or implement it.
type AuditSyncHandler interface {
	SyncAudit(context.Context, SessionInfo, AuditSyncStream) error
}

type AuditRecord struct {
	Incarnation uint64
	Sequence    uint64
	AppendedAt  time.Time
	Payload     []byte
}

type AuditAck struct {
	AuditArchiveID       string
	Incarnation          uint64
	Sequence             uint64
	CoverageRevisionSeen uint64
}

type AuditGap struct {
	Incarnation      uint64
	FromSequence     uint64
	UntilSequence    uint64
	Reason           string
	Precision        string
	LastLossRevision uint64
}

type AuditCoverageSnapshot struct {
	Revision                    uint64
	GeneratedAt                 time.Time
	Gaps                        []AuditGap
	CoverageUnknownIncarnations []uint64
}

type AuditBounds struct {
	WALFloor              *AuditCursor
	WALCeiling            *AuditCursor
	NextCursor            AuditCursor
	ServerACKedThrough    *AuditCursor
	AcknowledgedArchiveID string
	CoverageRevision      uint64
}

type AuditCursor struct {
	Incarnation uint64
	Sequence    uint64
}

type AuditCursorBehindFloor struct {
	Requested AuditCursor
	Bounds    AuditBounds
	Coverage  AuditCoverageSnapshot
}

type AuditAckResult struct {
	Proposed      AuditCursor
	Accepted      bool
	StaleCoverage *AuditCoverageSnapshot
	Error         string
}

// AuditUpstream contains exactly one Agent-to-Server durable-sync fact.
type AuditUpstream struct {
	Record            *AuditRecord
	Coverage          *AuditCoverageSnapshot
	CursorBehindFloor *AuditCursorBehindFloor
	AckResult         *AuditAckResult
}

type AuditSyncStream interface {
	Context() context.Context
	Send(AuditUpstream) error
	ReceiveAck() (AuditAck, error)
}

// AuditReceiveStream is the Server view of the same P1 bidirectional stream.
// It is intentionally separate from ControlSession so narrow test/session
// implementations need not claim durable-sync support.
type AuditReceiveStream interface {
	Recv(context.Context) (AuditUpstream, error)
	SendAck(AuditAck) error
	Close() error
}

type AuditControlSession interface {
	ControlSession
	OpenAuditSync(context.Context) (AuditReceiveStream, error)
}

// OperationControlSession extends the stable session surface without forcing
// older Server call sites and protocol N-1 test doubles to claim support.
type OperationControlSession interface {
	ControlSession
	GetOperation(context.Context, GetOperationRequest) (GetOperationResponse, error)
	CancelOperation(context.Context, CancelOperationRequest) (CancelOperationResponse, error)
}

// OperationRecoverySession is an optional N capability. Protocol N-1 sessions
// still expose this Go interface through the concrete transport, but calls are
// answered with ErrHandlerUnavailable by an N-1 Agent.
type OperationRecoverySession interface {
	ControlSession
	ListActiveOperations(context.Context, ListActiveOperationsRequest) (ListActiveOperationsResponse, error)
}

type Session interface {
	Info() SessionInfo
	Done() <-chan struct{}
	Err() error
	Close(error) error
}

type State string

const (
	StateActive  State = "ACTIVE"
	StateOffline State = "OFFLINE"
	StateClosed  State = "CLOSED"
)

type ControlSession interface {
	Session
	Heartbeat(context.Context) (Heartbeat, error)
	Query(context.Context, QueryRequest) (QueryResponse, error)
	StartOperation(context.Context, OperationRequest) (OperationResponse, error)
	OpenLogs(context.Context, LogRequest) (LogReceiveStream, error)
	OpenStats(context.Context, StatsRequest) (StatsReceiveStream, error)
	State() State
	LastHeartbeat() time.Time
	Do(context.Context, TrafficClass, func(context.Context) error) error
}

type Clock interface {
	Now() time.Time
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TickerFactory interface {
	NewTicker(time.Duration) Ticker
}

type TickerFactoryFunc func(time.Duration) Ticker

func (f TickerFactoryFunc) NewTicker(interval time.Duration) Ticker { return f(interval) }

type LivenessObserver func(SessionInfo, State, error)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type realTicker struct{ ticker *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

type realTickerFactory struct{}

func (realTickerFactory) NewTicker(interval time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(interval)}
}

type sessionCore struct {
	info SessionInfo
	done chan struct{}
	once sync.Once
	mu   sync.RWMutex
	err  error
}

func newSessionCore(info SessionInfo) sessionCore {
	return sessionCore{info: info, done: make(chan struct{})}
}

func (s *sessionCore) Info() SessionInfo     { return s.info }
func (s *sessionCore) Done() <-chan struct{} { return s.done }
func (s *sessionCore) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}
func (s *sessionCore) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}
