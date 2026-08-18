// Package workload provides the synthetic Agent used by every transport
// candidate. It calls no Docker, Compose, filesystem, or database API.
package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/east-true/dockpilot/internal/contract"
	pb "github.com/east-true/dockpilot/internal/contract/pb"
	"github.com/east-true/dockpilot/internal/metrics"
)

type Config struct {
	AgentID                 string
	AuditRecordsPerSecond   int
	AuditPayloadBytes       int
	AuditQueueRecords       int
	AuditMode               string
	OperationDuration       time.Duration
	OperationLinesPerSecond int
	OperationLineBytes      int
	StatsPaddingBytes       int
}

func DefaultConfig() Config {
	return Config{
		AgentID:                 "agent-1",
		AuditRecordsPerSecond:   20,
		AuditPayloadBytes:       512, // target serialized record size
		AuditQueueRecords:       20000,
		AuditMode:               "managed-like",
		OperationDuration:       120 * time.Second,
		OperationLinesPerSecond: 50,
		OperationLineBytes:      200,
		StatsPaddingBytes:       1024, // target serialized sample size
	}
}

type Service struct {
	prototypeOnly // compile-time guard against accidental production reuse; see marker.go

	cfg Config
	reg *metrics.Registry

	ctx    context.Context
	cancel context.CancelFunc
	auditQ chan *pb.AuditRecord
	seq    atomic.Uint64

	opMu sync.Mutex
	ops  map[string]context.CancelFunc

	sessionMu    sync.RWMutex
	sessionID    string
	sessionReady chan struct{}
	bindOnce     sync.Once
}

func NewService(cfg Config, reg *metrics.Registry) *Service {
	if cfg.AuditQueueRecords <= 0 {
		cfg.AuditQueueRecords = 20000
	}
	if cfg.AuditPayloadBytes <= 0 {
		cfg.AuditPayloadBytes = 512
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "agent-1"
	}
	if cfg.AuditMode == "" {
		cfg.AuditMode = "managed-like"
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, reg: reg, ctx: ctx, cancel: cancel, auditQ: make(chan *pb.AuditRecord, cfg.AuditQueueRecords), ops: make(map[string]context.CancelFunc), sessionReady: make(chan struct{})}
	if cfg.AuditRecordsPerSecond > 0 {
		go s.generateAudit()
	}
	return s
}

func (s *Service) Close() { s.cancel() }

// BindSession binds the logical Register response to the transport handshake.
// It is one-shot because a Service instance belongs to exactly one connection.
func (s *Service) BindSession(id string) {
	s.bindOnce.Do(func() {
		s.sessionMu.Lock()
		s.sessionID = id
		s.sessionMu.Unlock()
		close(s.sessionReady)
	})
}

func (s *Service) generateAudit() {
	interval := time.Second / time.Duration(s.cfg.AuditRecordsPerSecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			seq := s.seq.Add(1)
			record := &pb.AuditRecord{AgentId: s.cfg.AgentID, Incarnation: 1, Seq: seq, OccurredAtUnixNano: now.UnixNano(), Kind: s.cfg.AuditMode}
			padProto(record, s.cfg.AuditPayloadBytes, func(n int) { record.Payload = make([]byte, n) })
			size := float64(proto.Size(record))
			s.reg.AddGauge(metrics.BufferBytes, size)
			select {
			case s.auditQ <- record:
				s.reg.Add(metrics.AuditGeneratedTotal, 1)
			case <-s.ctx.Done():
				s.reg.AddGauge(metrics.BufferBytes, -size)
				return
			}
		}
	}
}

func (s *Service) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if req.AgentId != s.cfg.AgentID {
		return nil, fmt.Errorf("register agent_id %q does not match %q", req.AgentId, s.cfg.AgentID)
	}
	select {
	case <-s.sessionReady:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	s.sessionMu.RLock()
	sessionID := s.sessionID
	s.sessionMu.RUnlock()
	if sessionID == "" {
		return nil, errors.New("transport session id is empty")
	}
	return &pb.RegisterResponse{SessionId: sessionID, ProtocolVersion: req.ProtocolVersion, Capability: &pb.Capability{}}, nil
}

func (s *Service) Heartbeat(_ context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{SentAtUnixNano: req.SentAtUnixNano, Capability: &pb.Capability{}}, nil
}

func (s *Service) CancelOperation(_ context.Context, req *pb.CancelOperationRequest) (*pb.CancelOperationResponse, error) {
	start := time.Now()
	s.opMu.Lock()
	cancel, ok := s.ops[req.OperationId]
	if ok {
		cancel()
	}
	s.opMu.Unlock()
	s.reg.Observe(metrics.CancelAckLatencyMS, float64(time.Since(start).Microseconds())/1000)
	result := pb.CancelResult_CANCEL_RESULT_NOT_FOUND
	if ok {
		result = pb.CancelResult_CANCEL_RESULT_ACCEPTED
	}
	return &pb.CancelOperationResponse{Result: result, RequestedAtUnixNano: time.Now().UnixNano()}, nil
}

func (s *Service) OperationProgress(ctx context.Context, _ *pb.OperationProgressRequest, out contract.Sink[*pb.OperationProgressEvent]) error {
	operationID := "simulated-operation"
	started := time.Now()
	events := []struct {
		at    time.Duration
		phase pb.OperationPhase
	}{
		{0, pb.OperationPhase_OPERATION_PHASE_PREPARING},
		{s.cfg.OperationDuration / 20, pb.OperationPhase_OPERATION_PHASE_EXECUTING},
		{s.cfg.OperationDuration * 19 / 20, pb.OperationPhase_OPERATION_PHASE_FINALIZING},
		{s.cfg.OperationDuration, pb.OperationPhase_OPERATION_PHASE_TERMINAL},
	}
	for i, event := range events {
		if err := waitUntil(ctx, started.Add(event.at)); err != nil {
			return err
		}
		now := time.Now()
		if err := out.Send(ctx, &pb.OperationProgressEvent{OperationId: operationID, OperationRevision: uint64(i + 1), Phase: event.phase, CancelMode: pb.CancelMode_CANCEL_MODE_BEST_EFFORT_PARTIAL, OccurredAtUnixNano: now.UnixNano(), Terminal: event.phase == pb.OperationPhase_OPERATION_PHASE_TERMINAL, TerminalStatus: "success"}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) SyncAudit(ctx context.Context, stream contract.AuditSyncStream) error {
	ackErr := make(chan error, 1)
	go func() {
		for {
			ack, err := stream.Recv(ctx)
			if err != nil {
				ackErr <- err
				return
			}
			if ack.Cursor != nil {
				s.reg.Set(metrics.AuditAckCursor, float64(ack.Cursor.Seq))
				s.reg.Set(metrics.AuditCoverageRevisionSeen, float64(ack.CoverageRevisionSeen))
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ackErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case record := <-s.auditQ:
			err := stream.Send(ctx, &pb.SyncAuditMessage{Body: &pb.SyncAuditMessage_Record{Record: record}})
			s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(record)))
			if err != nil {
				return err
			}
			s.reg.Set(metrics.AuditSyncLagRecords, float64(len(s.auditQ)))
		}
	}
}

func (s *Service) GetAuditCoverage(context.Context, *pb.GetAuditCoverageRequest) (*pb.AuditCoverageSnapshot, error) {
	return &pb.AuditCoverageSnapshot{AgentId: s.cfg.AgentID, CoverageRevision: 1, GeneratedAtUnixNano: time.Now().UnixNano()}, nil
}

func (s *Service) Echo(_ context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	return &pb.EchoResponse{Payload: make([]byte, req.PayloadSize), SentAtUnixNano: req.SentAtUnixNano}, nil
}

func (s *Service) StreamLogs(ctx context.Context, req *pb.StreamLogsRequest, out contract.Sink[*pb.LogChunk]) error {
	lineSize := int(req.LineSize)
	if lineSize <= 0 {
		lineSize = 200
	}
	byteRate := int(req.ByteRate)
	if byteRate <= 0 {
		byteRate = 200 << 10
	}
	linesPerSecond := max(1, byteRate/lineSize)
	batchLines := min(16, linesPerSecond)
	interval := time.Duration(batchLines) * time.Second / time.Duration(linesPerSecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			for range batchLines {
				seq++
				if err := out.Send(ctx, &pb.LogChunk{StreamId: req.StreamId, Seq: seq, EmittedAtUnixNano: now.UnixNano(), Data: make([]byte, lineSize)}); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Service) OperationOutput(ctx context.Context, req *pb.OperationOutputRequest, out contract.Sink[*pb.OperationOutputChunk]) error {
	opCtx, cancel := context.WithCancel(ctx)
	s.opMu.Lock()
	s.ops[req.OperationId] = cancel
	s.opMu.Unlock()
	defer func() {
		cancel()
		s.opMu.Lock()
		delete(s.ops, req.OperationId)
		s.opMu.Unlock()
	}()
	lineBytes := max(1, s.cfg.OperationLineBytes)
	tail := make(chan *pb.OperationOutputChunk, max(1, (64<<10)/lineBytes))
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(tail)
		interval := time.Second / time.Duration(max(1, s.cfg.OperationLinesPerSecond))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		timer := time.NewTimer(s.cfg.OperationDuration)
		defer timer.Stop()
		var seq uint64
		for {
			select {
			case <-opCtx.Done():
				return
			case <-timer.C:
				return
			case now := <-ticker.C:
				seq++
				chunk := &pb.OperationOutputChunk{OperationId: req.OperationId, Seq: seq, EmittedAtUnixNano: now.UnixNano(), Data: make([]byte, lineBytes)}
				size := float64(proto.Size(chunk))
				s.reg.AddGauge(metrics.BufferBytes, size)
				select {
				case tail <- chunk:
				default:
					s.reg.AddGauge(metrics.BufferBytes, -size)
					// Always drain the simulated CLI producer. Retain a bounded
					// newest tail and mark the omission explicitly.
					select {
					case old := <-tail:
						s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(old)))
					default:
					}
					chunk.Truncated = true
					s.reg.Add(metrics.OperationOutputTruncatedTotal, 1)
					size = float64(proto.Size(chunk))
					s.reg.AddGauge(metrics.BufferBytes, size)
					select {
					case tail <- chunk:
					default:
						s.reg.AddGauge(metrics.BufferBytes, -size)
					}
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-producerDone
		for chunk := range tail {
			s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(chunk)))
		}
	}()
	for {
		select {
		case <-opCtx.Done():
			<-producerDone
			return opCtx.Err()
		case chunk, ok := <-tail:
			if !ok {
				return nil
			}
			err := out.Send(opCtx, chunk)
			s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(chunk)))
			if err != nil {
				return err
			}
		}
	}
}

func (s *Service) StreamStats(ctx context.Context, req *pb.StreamStatsRequest, out contract.Sink[*pb.StatsSample]) error {
	statsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interval := time.Duration(req.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ready := make(chan struct{}, 1)
	var latestMu sync.Mutex
	latest := make(map[string]*pb.StatsSample, len(req.Targets))
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var n uint64
		for {
			select {
			case <-statsCtx.Done():
				return
			case now := <-ticker.C:
				n++
				for _, target := range req.Targets {
					sample := &pb.StatsSample{Target: target, SampledAtUnixNano: now.UnixNano(), CpuPercent: math.Mod(float64(n), 100), MemoryBytes: n * 1024, MemoryLimitBytes: 512 << 20}
					padProto(sample, s.cfg.StatsPaddingBytes, func(size int) { sample.Padding = make([]byte, size) })
					latestMu.Lock()
					if old, exists := latest[target]; exists {
						s.reg.Add(metrics.StatsSamplesDroppedTotal, 1)
						s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(old)))
					}
					latest[target] = sample
					s.reg.AddGauge(metrics.BufferBytes, float64(proto.Size(sample)))
					latestMu.Unlock()
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-producerDone
		latestMu.Lock()
		for target, sample := range latest {
			s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(sample)))
			delete(latest, target)
		}
		latestMu.Unlock()
	}()
	for {
		select {
		case <-statsCtx.Done():
			return statsCtx.Err()
		case <-ready:
			latestMu.Lock()
			batch := make([]*pb.StatsSample, 0, len(latest))
			for target, sample := range latest {
				batch = append(batch, sample)
				delete(latest, target)
			}
			latestMu.Unlock()
			for i, sample := range batch {
				if err := out.Send(statsCtx, sample); err != nil {
					for _, remaining := range batch[i:] {
						s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(remaining)))
					}
					return err
				}
				s.reg.AddGauge(metrics.BufferBytes, -float64(proto.Size(sample)))
				s.reg.Add(metrics.StatsSamplesSentTotal, 1)
			}
		}
	}
}

func padProto(msg proto.Message, target int, set func(int)) {
	if target <= 0 {
		return
	}
	padding := max(0, target-proto.Size(msg)-3)
	for attempts := 0; attempts < 4; attempts++ {
		set(padding)
		delta := target - proto.Size(msg)
		if delta == 0 {
			return
		}
		padding = max(0, padding+delta)
	}
}

func waitUntil(ctx context.Context, at time.Time) error {
	d := time.Until(at)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
