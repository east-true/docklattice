// Package agentproduct assembles the Agent's complete product transport
// surface from its independently testable query, operation, log, and stats
// implementations. Durable identity, heartbeat, and Audit sync remain owned by
// agentruntime and are delegated through Control.
package agentproduct

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/agentops"
	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/agentquery"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeexec"
	productconfig "github.com/east-true/dockpilot/internal/config"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/livematrix"
	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
	"github.com/east-true/dockpilot/internal/operation"
	"github.com/east-true/dockpilot/internal/producttransport"
)

// Docker is the synchronous query/mutation boundary. Live sources are passed
// separately because dockeradapter creates them only after confirming that the
// underlying Engine client implements the corresponding streaming API.
type Docker interface {
	agentquery.DockerReader
	agentops.Docker
}

// Projects is the single in-memory catalog used for both read snapshots and
// mutation target resolution. This prevents Query and Operation handlers from
// observing different project capability decisions.
type Projects interface {
	agentquery.ProjectCatalog
	agentops.ProjectCatalog
	agentops.FileApprovalCatalog
	agentops.FilesystemPolicy
	agentops.Rescanner
}

// Backups keeps backup bytes Agent-owned while exposing only metadata through
// the query path.
type Backups interface {
	agentquery.BackupMetadataLister
	agentops.BackupManager
}

var (
	_ Docker                = (*dockeradapter.Adapter)(nil)
	_ Projects              = (*agentprojects.Catalog)(nil)
	_ Backups               = (*backup.Manager)(nil)
	_ agentops.Compose      = composeexec.Runner{}
	_ agentquery.FileReader = (*agentquery.SafeFiles)(nil)
)

// Config contains already-opened, lifecycle-owned dependencies. New does not
// open Docker, scan host paths, or create durable stores; agentruntime owns
// those boot-order decisions and passes their verified handles here.
type Config struct {
	Control producttransport.AgentHandler

	Docker    Docker
	Projects  Projects
	Files     agentquery.FileReader
	Backups   Backups
	Engine    *operation.Engine
	Compose   agentops.Compose
	Admission agentops.DiskAdmitter
	Timeouts  productconfig.OperationTimeouts

	LogSource         logrelay.Source
	LogBytesPerSecond int64
	LogBufferedBytes  int
	LogBufferedChunks int

	StatsSource         livestats.Source
	StatsSampleInterval time.Duration

	// MatrixDocker, MatrixPaths and MatrixFrameInterval configure host-scoped
	// live metrics. MatrixPaths are the paths Dockpilot writes to - the
	// discovery roots and the Agent state root - reported as capacity, not as
	// an inventory of the host's mounts.
	MatrixDocker        MatrixDocker
	MatrixPaths         []string
	MatrixFrameInterval time.Duration

	// matrixEventRetry and matrixProbe exist so tests can drive the event
	// resubscribe delay and the filesystem probe without a real host.
	matrixEventRetry time.Duration
	matrixProbe      func(string) (filesystemUsage, error)
}

// Handler implements the complete v1 Agent product surface. It owns only the
// live-stats hub created by New; all durable and Docker dependencies remain
// owned by the caller and must outlive Handler.
type Handler struct {
	control    producttransport.AgentHandler
	audit      producttransport.AuditSyncHandler
	query      producttransport.QueryHandler
	operations producttransport.OperationHandler
	engine     *operation.Engine
	logs       producttransport.LogStreamHandler
	stats      producttransport.StatsStreamHandler
	statsHub   *livestats.Hub
	matrix     producttransport.MetricsMatrixStreamHandler
	matrixHub  *livematrix.Hub

	closeOnce sync.Once
	closeErr  error
}

var (
	_ producttransport.AgentHandler               = (*Handler)(nil)
	_ producttransport.AuditSyncHandler           = (*Handler)(nil)
	_ producttransport.QueryHandler               = (*Handler)(nil)
	_ producttransport.OperationHandler           = (*Handler)(nil)
	_ producttransport.OperationControlHandler    = (*Handler)(nil)
	_ producttransport.OperationRecoveryHandler   = (*Handler)(nil)
	_ producttransport.LogStreamHandler           = (*Handler)(nil)
	_ producttransport.StatsStreamHandler         = (*Handler)(nil)
	_ producttransport.MetricsMatrixStreamHandler = (*Handler)(nil)
)

func New(config Config) (*Handler, error) {
	if config.Control == nil {
		return nil, errors.New("agentproduct: control handler is required")
	}
	audit, ok := config.Control.(producttransport.AuditSyncHandler)
	if !ok {
		return nil, errors.New("agentproduct: control handler must provide durable Audit sync")
	}
	if config.Docker == nil || config.Projects == nil || config.Files == nil || config.Backups == nil ||
		config.Engine == nil || config.Compose == nil || config.Admission == nil ||
		config.LogSource == nil || config.StatsSource == nil {
		return nil, errors.New("agentproduct: Docker, projects, files, backups, operation engine, Compose, admission, log source, and stats source are required")
	}

	queries, err := agentquery.New(agentquery.Config{
		Docker: config.Docker, Projects: config.Projects, Files: config.Files, Backups: config.Backups, Compose: config.Compose,
	})
	if err != nil {
		return nil, err
	}
	operations, err := agentops.New(agentops.Config{
		Engine: config.Engine, Docker: config.Docker, Compose: config.Compose,
		Projects: config.Projects, Approvals: config.Projects, Filesystem: config.Projects, Rescanner: config.Projects, Backups: config.Backups,
		Admission: config.Admission, Timeouts: config.Timeouts,
	})
	if err != nil {
		return nil, err
	}
	logs, err := logrelay.New(logrelay.Config{
		Source:           composeLogSource{docker: config.LogSource, inventory: config.Docker, compose: config.Compose, projects: config.Projects},
		BytesPerSecond:   config.LogBytesPerSecond,
		MaxBufferedBytes: config.LogBufferedBytes, MaxBufferedChunks: config.LogBufferedChunks,
	})
	if err != nil {
		return nil, err
	}
	stats, err := livestats.New(livestats.Config{
		Source: config.StatsSource, SampleInterval: config.StatsSampleInterval,
	})
	if err != nil {
		return nil, err
	}
	matrix, err := newMatrixHub(config, stats)
	if err != nil {
		_ = stats.Close()
		return nil, err
	}
	return &Handler{
		control: config.Control, audit: audit, query: queries, operations: operations,
		engine: config.Engine,
		logs:   producttransport.LogRelayHandler{Relay: logs},
		stats:  producttransport.LiveStatsHandler{Hub: stats}, statsHub: stats,
		matrix: producttransport.LiveMatrixHandler{Hub: matrix}, matrixHub: matrix,
	}, nil
}

func (h *Handler) Heartbeat(ctx context.Context, info producttransport.SessionInfo, sentAt time.Time) (producttransport.Capability, error) {
	return h.control.Heartbeat(ctx, info, sentAt)
}

func (h *Handler) SyncAudit(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditSyncStream) error {
	return h.audit.SyncAudit(ctx, info, stream)
}

func (h *Handler) Query(ctx context.Context, info producttransport.SessionInfo, request producttransport.QueryRequest) (producttransport.QueryResponse, error) {
	return h.query.Query(ctx, info, request)
}

func (h *Handler) StartOperation(ctx context.Context, info producttransport.SessionInfo, request producttransport.OperationRequest) (producttransport.OperationResponse, error) {
	return h.operations.StartOperation(ctx, info, request)
}

func (h *Handler) GetOperation(_ context.Context, _ producttransport.SessionInfo, request producttransport.GetOperationRequest) (producttransport.GetOperationResponse, error) {
	if err := validateOperationControlID(request.OperationID); err != nil {
		return producttransport.GetOperationResponse{}, err
	}
	record, found := h.engine.Get(request.OperationID)
	if !found {
		return producttransport.GetOperationResponse{Found: false}, nil
	}
	return producttransport.GetOperationResponse{Found: true, Operation: operationResponse(record)}, nil
}

func (h *Handler) CancelOperation(_ context.Context, _ producttransport.SessionInfo, request producttransport.CancelOperationRequest) (producttransport.CancelOperationResponse, error) {
	if err := validateOperationControlID(request.OperationID); err != nil {
		return producttransport.CancelOperationResponse{}, err
	}
	reason := operation.CancelReason(request.Reason)
	if !validCancelReason(reason) {
		return producttransport.CancelOperationResponse{}, fmt.Errorf("agentproduct: unsupported cancel reason %q", request.Reason)
	}
	outcome, err := h.engine.CancelWithError(request.OperationID, reason)
	if err != nil {
		return producttransport.CancelOperationResponse{}, err
	}
	response := producttransport.CancelOperationResponse{Outcome: string(outcome)}
	if outcome == operation.CancelNotFound {
		return response, nil
	}
	record, found := h.engine.Get(request.OperationID)
	if !found {
		return producttransport.CancelOperationResponse{}, errors.New("agentproduct: canceled operation disappeared from the result index")
	}
	response.Operation = operationResponse(record)
	return response, nil
}

func (h *Handler) ListActiveOperations(_ context.Context, _ producttransport.SessionInfo, _ producttransport.ListActiveOperationsRequest) (producttransport.ListActiveOperationsResponse, error) {
	records := h.engine.ListActiveOperations()
	if len(records) > producttransport.MaxActiveOperationCount {
		return producttransport.ListActiveOperationsResponse{}, fmt.Errorf("agentproduct: active operation count %d exceeds protocol limit %d", len(records), producttransport.MaxActiveOperationCount)
	}
	operations := make([]producttransport.ActiveOperation, 0, len(records))
	for _, record := range records {
		if err := validateOperationControlID(record.OperationID); err != nil {
			return producttransport.ListActiveOperationsResponse{}, err
		}
		if len(record.OutputTail) > producttransport.MaxOperationOutputTailBytes {
			return producttransport.ListActiveOperationsResponse{}, fmt.Errorf("agentproduct: operation %q output tail exceeds protocol limit", record.OperationID)
		}
		operations = append(operations, producttransport.ActiveOperation{
			OperationID: record.OperationID, Type: string(record.Type), ProjectKey: record.ProjectKey,
			Target: record.Target, Operation: operationResponse(record),
		})
	}
	return producttransport.ListActiveOperationsResponse{Operations: operations}, nil
}

func operationResponse(record operation.Record) producttransport.OperationResponse {
	canCancel, cancelReason := operationRecordCancelability(record)
	return producttransport.OperationResponse{
		Status: string(record.Status), Phase: string(record.Phase), Revision: record.Revision,
		PartialEffectsPossible: record.PartialEffectsPossible, Error: record.Error,
		OutputTail: append([]byte(nil), record.OutputTail...), OutputTruncated: record.OutputTruncated,
		CancelMode: string(record.CancelMode), CanCancel: canCancel, CancelabilityReason: cancelReason,
		RequestedAt: record.RequestedAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
	}
}

func operationRecordCancelability(record operation.Record) (bool, string) {
	if record.Status.Terminal() {
		return false, "operation is terminal"
	}
	if !record.CancelRequestedAt.IsZero() {
		return false, "cancellation already requested"
	}
	if record.CancelMode == operation.CancelNone {
		return false, "operation is not cancelable"
	}
	if record.CancelMode == operation.CancelBeforeCommit && !record.CommitStartedAt.IsZero() {
		return false, "commit has started"
	}
	return true, ""
}

func validateOperationControlID(operationID string) error {
	if operationID == "" || len(operationID) > producttransport.MaxOperationIDBytes || !utf8.ValidString(operationID) {
		return fmt.Errorf("agentproduct: operation_id must be valid UTF-8 between 1 and %d bytes", producttransport.MaxOperationIDBytes)
	}
	return nil
}

func validCancelReason(reason operation.CancelReason) bool {
	switch reason {
	case operation.CancelReasonUser, operation.CancelReasonTimeout, operation.CancelReasonAgentShutdown:
		return true
	default:
		return false
	}
}

// The three stream methods below each check their handler before calling it.
//
// New cannot produce a Handler with any of them missing - it refuses the
// configuration instead - so in a running Agent these checks never fire. They
// are here because *Handler satisfies every stream handler interface
// unconditionally, which means the transport's own "handler is not configured"
// check can never fire for this type: the type assertion always succeeds, and
// whatever is behind the field is called. That leaves the constructor as the
// only thing standing between a Server's call and a nil dereference, and a
// process boundary must not depend on an invariant held one layer away.
//
// Capability negotiation decides which Agents a Server should call. It is not
// what keeps an Agent alive when something calls anyway - an older Server, a
// replayed request, a future build that makes one of these optional. The answer
// to a call that cannot be served is the same one every other unconfigured
// handler gives, which the transport already renders as Unimplemented.

func (h *Handler) StreamLogs(ctx context.Context, info producttransport.SessionInfo, request producttransport.LogRequest, sender producttransport.LogSender) error {
	if h.logs == nil {
		return fmt.Errorf("%w: logs", producttransport.ErrHandlerUnavailable)
	}
	return h.logs.StreamLogs(ctx, info, request, sender)
}

func (h *Handler) StreamStats(ctx context.Context, info producttransport.SessionInfo, request producttransport.StatsRequest, sender producttransport.StatsSender) error {
	if h.stats == nil {
		return fmt.Errorf("%w: stats", producttransport.ErrHandlerUnavailable)
	}
	return h.stats.StreamStats(ctx, info, request, sender)
}

func (h *Handler) StreamMetricsMatrix(ctx context.Context, info producttransport.SessionInfo, request producttransport.MetricsMatrixRequest, sender producttransport.MetricsMatrixSender) error {
	if h.matrix == nil {
		return fmt.Errorf("%w: metrics matrix", producttransport.ErrHandlerUnavailable)
	}
	return h.matrix.StreamMetricsMatrix(ctx, info, request, sender)
}

// Close stops viewer-scoped Docker stats collection. It deliberately does not
// close Docker, the operation journal, backup storage, or project roots because
// those dependencies are owned by agentruntime and have a stricter shutdown
// order relative to Audit WAL finalization.
func (h *Handler) Close() error {
	// The matrix hub holds container subscriptions from the stats hub, so it
	// goes first: closing it releases them before their owner shuts down.
	h.closeOnce.Do(func() { h.closeErr = errors.Join(h.matrixHub.Close(), h.statsHub.Close()) })
	return h.closeErr
}
