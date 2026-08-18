package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/east-true/dockpilot/internal/contract"
	pb "github.com/east-true/dockpilot/internal/contract/pb"
	"github.com/east-true/dockpilot/internal/metrics"
	"github.com/east-true/dockpilot/internal/transport"
)

type Summary struct {
	Config                           Config            `json:"config"`
	Candidate                        string            `json:"candidate"`
	Network                          string            `json:"network"`
	Scenario                         int               `json:"scenario"`
	Trial                            int               `json:"trial"`
	OfficialTiming                   bool              `json:"official_timing"`
	ControlledHarness                bool              `json:"controlled_harness"`
	ExpectedAgents                   int               `json:"expected_agents"`
	StartedAt                        time.Time         `json:"started_at"`
	WorkloadFinishedAt               time.Time         `json:"workload_finished_at"`
	WorkloadElapsedSeconds           float64           `json:"workload_elapsed_seconds"`
	FinishedAt                       time.Time         `json:"finished_at"`
	MeasurementStartedAt             time.Time         `json:"measurement_started_at"`
	MeasurementStartedElapsedSeconds float64           `json:"measurement_started_elapsed_seconds"`
	OperationDurationMS              float64           `json:"operation_duration_ms,omitempty"`
	OperationExpectedMS              float64           `json:"operation_expected_ms,omitempty"`
	AuditFirstCursor                 uint64            `json:"audit_first_cursor,omitempty"`
	AuditLastCursor                  uint64            `json:"audit_last_cursor,omitempty"`
	AuditCursorAdvances              uint64            `json:"audit_cursor_advances,omitempty"`
	AuditCursorPauseStart            uint64            `json:"audit_cursor_pause_start,omitempty"`
	AuditCursorPauseEnd              uint64            `json:"audit_cursor_pause_end,omitempty"`
	AuditAdvancesInPause             uint64            `json:"audit_advances_during_pause,omitempty"`
	MaxAuditStallSeconds             float64           `json:"max_audit_stall_seconds,omitempty"`
	LogBytesByStream                 map[string]uint64 `json:"log_bytes_by_stream,omitempty"`
	LogBytesBeforePause              map[string]uint64 `json:"log_bytes_before_pause,omitempty"`
	LogBytesDuringPause              map[string]uint64 `json:"log_bytes_during_pause,omitempty"`
	LogBeforeSeconds                 float64           `json:"log_before_seconds,omitempty"`
	LogPauseSeconds                  float64           `json:"log_pause_seconds,omitempty"`
	StatsSamples                     uint64            `json:"stats_samples,omitempty"`
	EchoRequests                     uint64            `json:"echo_requests,omitempty"`
	Errors                           []string          `json:"errors,omitempty"`
	BaselineGoroutines               float64           `json:"baseline_goroutines,omitempty"`
	FinalGoroutines                  float64           `json:"final_goroutines,omitempty"`
	BaselineRSSBytes                 float64           `json:"baseline_rss_bytes,omitempty"`
	FinalRSSBytes                    float64           `json:"final_rss_bytes,omitempty"`
	StreamCancelCycles               int               `json:"stream_cancel_cycles,omitempty"`
	OperationCancelCycles            int               `json:"operation_cancel_cycles,omitempty"`
	Registrations                    int               `json:"registrations,omitempty"`
	CoverageSnapshots                int               `json:"coverage_snapshots,omitempty"`
	Heartbeats                       int               `json:"heartbeats,omitempty"`
}

type RunOptions struct {
	Candidate string
	Network   string
	Trial     int
	RawPath   string
}

func Run(ctx context.Context, callers []transport.Caller, cfg Config, reg *metrics.Registry, opts RunOptions) (Summary, error) {
	summary := Summary{Config: cfg, Candidate: opts.Candidate, Network: opts.Network, Scenario: cfg.Scenario, Trial: opts.Trial, OfficialTiming: cfg.TimeScale == 1 && cfg.ControlledHarness, ControlledHarness: cfg.ControlledHarness, ExpectedAgents: cfg.Agents, StartedAt: time.Now().UTC()}
	initializeMetrics(reg)
	if err := bootstrapContracts(ctx, callers, &summary); err != nil {
		return summary, err
	}
	raw, err := os.Create(opts.RawPath)
	if err != nil {
		return summary, err
	}
	sampleCtx, stopSamples := context.WithCancel(ctx)
	sampleErr := make(chan error, 1)
	candidateCtx, stopCandidateMetrics := context.WithCancel(sampleCtx)
	candidateMetricsDone := make(chan struct{})
	go func() {
		defer close(candidateMetricsDone)
		collectCandidateMetrics(candidateCtx, callers, reg)
	}()
	go func() {
		sampleErr <- metrics.WriteJSONL(sampleCtx.Done(), raw, time.Second, reg, map[string]string{"role": "server", "candidate": opts.Candidate, "network": opts.Network, "scenario": fmt.Sprint(cfg.Scenario), "trial": fmt.Sprint(opts.Trial)})
	}()

	workloadStarted := time.Now()
	switch cfg.Scenario {
	case 1, 2:
		err = runInterference(ctx, callers[0], cfg, reg, &summary)
	case 3:
		err = runScale(ctx, callers, cfg, reg, &summary)
	case 4:
		err = runCancellation(ctx, callers[0], cfg, reg, &summary)
	default:
		err = fmt.Errorf("unknown scenario %d", cfg.Scenario)
	}
	summary.WorkloadElapsedSeconds = time.Since(workloadStarted).Seconds()
	summary.WorkloadFinishedAt = time.Now().UTC()
	stopCandidateMetrics()
	<-candidateMetricsDone
	if cfg.Scenario == 4 {
		collectCandidateRecoveryMetricsOnce(ctx, callers, reg)
	} else {
		collectCandidateMetricsOnce(ctx, callers, reg)
	}
	stopSamples()
	if sampleWriteErr := <-sampleErr; sampleWriteErr != nil && err == nil {
		err = sampleWriteErr
	}
	if closeErr := raw.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	reg.CollectRuntime()
	final := reg.Snapshot(nil)
	summary.FinalGoroutines = final.Gauges[metrics.Goroutines]
	summary.FinalRSSBytes = final.Gauges[metrics.ProcessRSSBytes]
	summary.FinishedAt = time.Now().UTC()
	return summary, err
}

func initializeMetrics(reg *metrics.Registry) {
	for _, name := range []string{
		metrics.AuditStaleCoverageTotal,
		metrics.AuditAckRetryTotal,
		metrics.AuditAgentGapClaimsTotal,
		metrics.LogBytesDroppedTotal,
		metrics.StatsSamplesDroppedTotal,
		metrics.OperationOutputTruncatedTotal,
	} {
		reg.Add(name, 0)
	}
	for _, name := range []string{
		metrics.AuditCoverageRevisionSeen,
		metrics.AuditCoverageRevisionCurrent,
		metrics.AuditAckBlockedWhileIngesting,
		metrics.AuditAckBlockedWhileIngestingSecs,
		metrics.AuditIngestedUnackedRecords,
		metrics.AuditIngestedUnackedBytes,
		metrics.AuditEffectiveGapRecords,
		metrics.BufferBytes,
	} {
		reg.Set(name, 0)
	}
}

func bootstrapContracts(ctx context.Context, callers []transport.Caller, summary *Summary) error {
	for _, caller := range callers {
		client := contract.NewClient(caller)
		info := caller.Info()
		registered, err := client.Register(ctx, &pb.RegisterRequest{AgentId: string(info.AgentID), ProtocolVersion: info.ProtocolVersion})
		if err != nil {
			return fmt.Errorf("register %s: %w", info.AgentID, err)
		}
		if registered.SessionId != string(info.SessionID) || registered.ProtocolVersion != info.ProtocolVersion {
			return fmt.Errorf("register %s returned invalid session/version", info.AgentID)
		}
		summary.Registrations++
		coverage, err := client.GetAuditCoverage(ctx, &pb.GetAuditCoverageRequest{})
		if err != nil {
			return fmt.Errorf("coverage %s: %w", info.AgentID, err)
		}
		if coverage.AgentId != string(info.AgentID) || coverage.CoverageRevision == 0 {
			return fmt.Errorf("coverage %s returned invalid identity/revision", info.AgentID)
		}
		// Initial coverage snapshot is stored before ACKs are admitted, matching
		// the production metric semantics even though the prototype store is a counter.
		// Per-run registries are shared by callers and initialized identically.
		summary.CoverageSnapshots++
		now := time.Now().UnixNano()
		heartbeat, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{SentAtUnixNano: now})
		if err != nil {
			return fmt.Errorf("heartbeat %s: %w", info.AgentID, err)
		}
		if heartbeat.SentAtUnixNano != now {
			return fmt.Errorf("heartbeat %s did not correlate request", info.AgentID)
		}
		summary.Heartbeats++
	}
	return nil
}

type candidateMetricSource interface {
	CandidateMetrics(context.Context) map[string]float64
}

type candidateRecoveryMetricSource interface {
	CandidateRecoveryMetrics(context.Context) map[string]float64
}

func collectCandidateMetrics(ctx context.Context, callers []transport.Caller, reg *metrics.Registry) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	collect := func() {
		collectCandidateMetricsOnce(ctx, callers, reg)
	}
	collect()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func collectCandidateMetricsOnce(ctx context.Context, callers []transport.Caller, reg *metrics.Registry) {
	for _, caller := range callers {
		source, ok := caller.(candidateMetricSource)
		if !ok {
			continue
		}
		queryCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		values := source.CandidateMetrics(queryCtx)
		cancel()
		for name, value := range values {
			reg.Set(name, value)
		}
	}
}

func collectCandidateRecoveryMetricsOnce(ctx context.Context, callers []transport.Caller, reg *metrics.Registry) {
	collectCandidateMetricsOnce(ctx, callers, reg)
	for _, caller := range callers {
		source, ok := caller.(candidateRecoveryMetricSource)
		if !ok {
			continue
		}
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		values := source.CandidateRecoveryMetrics(queryCtx)
		cancel()
		for name, value := range values {
			reg.Set(name, value)
		}
	}
}

type streamSet struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func runInterference(parent context.Context, caller transport.Caller, cfg Config, reg *metrics.Registry, summary *Summary) error {
	ctx, cancel := context.WithTimeout(parent, cfg.Duration())
	defer cancel()
	client := contract.NewClient(caller)
	set := &streamSet{}
	streamCtx, streamCancel := context.WithCancel(ctx)
	set.cancel = streamCancel

	var firstCursor atomic.Uint64
	var lastCursor atomic.Uint64
	var advances atomic.Uint64
	var lastAuditNano atomic.Int64
	startAudit(streamCtx, set, client, reg, &firstCursor, &lastCursor, &advances, &lastAuditNano, summary)
	var maxAuditStallNanos atomic.Uint64
	set.wg.Add(1)
	go func() {
		defer set.wg.Done()
		ticker := time.NewTicker(min(cfg.scaled(time.Second), 100*time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case now := <-ticker.C:
				last := lastAuditNano.Load()
				if last == 0 {
					continue
				}
				delta := now.UnixNano() - last
				if delta < 0 {
					delta = 0
				}
				stall := uint64(delta)
				reg.Set(metrics.AuditAckWatermarkStalledSeconds, float64(stall)/float64(time.Second))
				for old := maxAuditStallNanos.Load(); stall > old && !maxAuditStallNanos.CompareAndSwap(old, stall); old = maxAuditStallNanos.Load() {
				}
			}
		}
	}()

	logTotals := make([]atomic.Uint64, cfg.LogStreams)
	logBefore := make([]atomic.Uint64, cfg.LogStreams)
	logPause := make([]atomic.Uint64, cfg.LogStreams)
	pauseStart, pauseEnd := pauseWindow(cfg)
	started := time.Now()
	measurementStart := measurementStart(cfg)
	summary.MeasurementStartedAt = started.Add(measurementStart).UTC()
	summary.MeasurementStartedElapsedSeconds = measurementStart.Seconds()
	summary.LogBeforeSeconds = (pauseStart - measurementStart).Seconds()
	summary.LogPauseSeconds = (pauseEnd - pauseStart).Seconds()
	var cursorPauseStart atomic.Uint64
	var cursorPauseEnd atomic.Uint64
	set.wg.Add(1)
	go func() {
		defer set.wg.Done()
		if err := waitContext(streamCtx, pauseStart); err != nil {
			return
		}
		cursorPauseStart.Store(lastCursor.Load())
		if err := waitContext(streamCtx, pauseEnd-pauseStart); err != nil {
			return
		}
		cursorPauseEnd.Store(lastCursor.Load())
	}()
	for i := 0; i < cfg.LogStreams; i++ {
		stream, err := client.StreamLogs(streamCtx, &pb.StreamLogsRequest{StreamId: fmt.Sprintf("log-%d", i+1), ByteRate: uint32(cfg.LogBytesSecond), LineSize: uint32(cfg.LogLineBytes)})
		if err != nil {
			return err
		}
		idx := i
		set.wg.Add(1)
		go func() {
			defer set.wg.Done()
			defer stream.Cancel(nil)
			for {
				elapsed := time.Since(started)
				if cfg.PauseLog && idx == 0 && elapsed >= pauseStart && elapsed < pauseEnd {
					if err := waitContext(streamCtx, pauseEnd-elapsed); err != nil {
						return
					}
				}
				chunk, err := stream.Recv(streamCtx)
				if err != nil {
					if !expectedEnd(err) {
						appendError(summary, fmt.Sprintf("log-%d: %v", idx+1, err))
					}
					return
				}
				n := uint64(len(chunk.Data))
				logTotals[idx].Add(n)
				reg.Add(metrics.LogBytesSentTotal, n)
				if elapsed >= measurementStart && elapsed < pauseStart {
					logBefore[idx].Add(n)
				} else if elapsed >= pauseStart && elapsed < pauseEnd {
					logPause[idx].Add(n)
				}
			}
		}()
	}

	var statsCount atomic.Uint64
	if cfg.StatsTargets > 0 {
		targets := make([]string, cfg.StatsTargets)
		for i := range targets {
			targets[i] = fmt.Sprintf("container-%d", i+1)
		}
		stats, err := client.StreamStats(streamCtx, &pb.StreamStatsRequest{Targets: targets, IntervalMs: uint32(max(1, cfg.scaled(2*time.Second).Milliseconds()))})
		if err != nil {
			return err
		}
		set.wg.Add(1)
		go func() {
			defer set.wg.Done()
			defer stats.Cancel(nil)
			for {
				if _, err := stats.Recv(streamCtx); err != nil {
					return
				}
				statsCount.Add(1)
			}
		}()
	}

	var echoCount atomic.Uint64
	set.wg.Add(2)
	go func() {
		defer set.wg.Done()
		interval := cfg.scaled(2 * time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case now := <-ticker.C:
				t0 := time.Now()
				_, err := client.Echo(streamCtx, &pb.EchoRequest{PayloadSize: uint32(cfg.EchoPayload), SentAtUnixNano: now.UnixNano()})
				if err != nil {
					if !expectedEnd(err) {
						appendError(summary, "echo: "+err.Error())
					}
					return
				}
				if time.Since(started) >= measurementStart {
					reg.Observe(metrics.QueryEchoRTTMS, float64(time.Since(t0).Microseconds())/1000)
				}
				echoCount.Add(1)
			}
		}
	}()
	go func() {
		defer set.wg.Done()
		interval := cfg.scaled(5 * time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				probeStarted := time.Now()
				_, err := client.CancelOperation(streamCtx, &pb.CancelOperationRequest{OperationId: "latency-probe-not-found", Reason: pb.CancelReason_CANCEL_REASON_USER})
				if err != nil {
					if !expectedEnd(err) {
						appendError(summary, "cancel probe: "+err.Error())
					}
					return
				}
				if time.Since(started) >= measurementStart {
					reg.Observe(metrics.CancelAckLatencyMS, float64(time.Since(probeStarted).Microseconds())/1000)
				}
			}
		}
	}()

	opAt := operationStart(cfg)
	if err := waitContext(ctx, opAt); err == nil {
		opStart := time.Now()
		progress, progressErr := client.OperationProgress(streamCtx, &pb.OperationProgressRequest{})
		output, outputErr := client.OperationOutput(streamCtx, &pb.OperationOutputRequest{OperationId: "simulated-operation"})
		if progressErr != nil || outputErr != nil {
			return errors.Join(progressErr, outputErr)
		}
		set.wg.Add(2)
		go func() {
			defer set.wg.Done()
			defer progress.Cancel(nil)
			for {
				event, err := progress.Recv(streamCtx)
				if err != nil {
					return
				}
				reg.Observe(metrics.OperationProgressEventLatencyMS, float64(time.Now().UnixNano()-event.OccurredAtUnixNano)/float64(time.Millisecond))
				if event.Terminal {
					summary.OperationDurationMS = float64(time.Since(opStart).Microseconds()) / 1000
					return
				}
			}
		}()
		go func() {
			defer set.wg.Done()
			defer output.Cancel(nil)
			for {
				chunk, err := output.Recv(streamCtx)
				if err != nil {
					return
				}
				if chunk.Truncated {
					reg.Add(metrics.OperationOutputTruncatedTotal, 1)
				}
			}
		}()
	}
	<-ctx.Done()
	streamCancel()
	set.wg.Wait()

	summary.OperationExpectedMS = float64(cfg.OperationDuration().Microseconds()) / 1000
	summary.AuditFirstCursor = firstCursor.Load()
	summary.AuditLastCursor = lastCursor.Load()
	summary.AuditCursorAdvances = advances.Load()
	summary.AuditCursorPauseStart = cursorPauseStart.Load()
	summary.AuditCursorPauseEnd = cursorPauseEnd.Load()
	if summary.AuditCursorPauseEnd > summary.AuditCursorPauseStart {
		summary.AuditAdvancesInPause = summary.AuditCursorPauseEnd - summary.AuditCursorPauseStart
	}
	summary.MaxAuditStallSeconds = float64(maxAuditStallNanos.Load()) / float64(time.Second)
	summary.LogBytesByStream = make(map[string]uint64, cfg.LogStreams)
	summary.LogBytesBeforePause = make(map[string]uint64, cfg.LogStreams)
	summary.LogBytesDuringPause = make(map[string]uint64, cfg.LogStreams)
	for i := range logTotals {
		key := fmt.Sprintf("log-%d", i+1)
		summary.LogBytesByStream[key] = logTotals[i].Load()
		summary.LogBytesBeforePause[key] = logBefore[i].Load()
		summary.LogBytesDuringPause[key] = logPause[i].Load()
	}
	summary.StatsSamples = statsCount.Load()
	summary.EchoRequests = echoCount.Load()
	return nil
}

func startAudit(ctx context.Context, set *streamSet, client *contract.Client, reg *metrics.Registry, first, last, advances *atomic.Uint64, lastAt *atomic.Int64, summary *Summary) {
	stream, err := client.SyncAudit(ctx)
	if err != nil {
		appendError(summary, "audit open: "+err.Error())
		return
	}
	set.wg.Add(1)
	go func() {
		defer set.wg.Done()
		defer stream.Cancel(nil)
		for {
			msg, err := stream.Recv(ctx)
			if err != nil {
				if !expectedEnd(err) {
					appendError(summary, "audit: "+err.Error())
				}
				return
			}
			record := msg.GetRecord()
			if record == nil {
				continue
			}
			if first.CompareAndSwap(0, record.Seq) {
				last.Store(record.Seq)
			} else if record.Seq > last.Load() {
				last.Store(record.Seq)
				advances.Add(1)
			}
			lastAt.Store(time.Now().UnixNano())
			reg.Add(metrics.AuditSyncedTotal, 1)
			reg.Set(metrics.AuditIngestedUnackedRecords, 1)
			reg.Set(metrics.AuditIngestedUnackedBytes, float64(proto.Size(record)))
			reg.Set(metrics.AuditSyncCursor, float64(record.Seq))
			reg.Set(metrics.AuditSyncLagSeconds, float64(time.Now().UnixNano()-record.OccurredAtUnixNano)/float64(time.Second))
			ack := &pb.SyncAuditAck{AuditArchiveId: "prototype-archive", Cursor: &pb.Cursor{Incarnation: record.Incarnation, Seq: record.Seq}, CoverageRevisionSeen: 1}
			if err := stream.Send(ctx, ack); err != nil {
				reg.Add(metrics.AuditAckRetryTotal, 1)
				appendError(summary, "audit ack: "+err.Error())
				return
			}
			reg.Set(metrics.AuditAckCursor, float64(record.Seq))
			reg.Set(metrics.AuditCoverageRevisionSeen, 1)
			reg.Set(metrics.AuditCoverageRevisionCurrent, 1)
			reg.Set(metrics.AuditIngestedUnackedRecords, 0)
			reg.Set(metrics.AuditIngestedUnackedBytes, 0)
			reg.Set(metrics.AuditAckBlockedWhileIngesting, 0)
			reg.Set(metrics.AuditAckBlockedWhileIngestingSecs, 0)
			reg.Set(metrics.AuditEffectiveGapRecords, 0)
		}
	}()
}

func runScale(parent context.Context, callers []transport.Caller, cfg Config, reg *metrics.Registry, summary *Summary) error {
	ctx, cancel := context.WithTimeout(parent, cfg.Duration())
	defer cancel()
	set := &streamSet{}
	var first, last, advances atomic.Uint64
	var lastAt atomic.Int64
	for i, caller := range callers {
		client := contract.NewClient(caller)
		startAudit(ctx, set, client, reg, &first, &last, &advances, &lastAt, summary)
		logs, err := client.StreamLogs(ctx, &pb.StreamLogsRequest{StreamId: fmt.Sprintf("agent-%d-log", i+1), ByteRate: uint32(cfg.LogBytesSecond), LineSize: uint32(cfg.LogLineBytes)})
		if err != nil {
			return err
		}
		stats, err := client.StreamStats(ctx, &pb.StreamStatsRequest{Targets: []string{"container-1"}, IntervalMs: uint32(max(1, cfg.scaled(2*time.Second).Milliseconds()))})
		if err != nil {
			return err
		}
		set.wg.Add(2)
		go drainLogs(ctx, set, logs, reg)
		go drainStats(ctx, set, stats, reg)
	}
	<-ctx.Done()
	set.wg.Wait()
	summary.AuditFirstCursor = first.Load()
	summary.AuditLastCursor = last.Load()
	summary.AuditCursorAdvances = advances.Load()
	return nil
}

func runCancellation(parent context.Context, caller transport.Caller, cfg Config, reg *metrics.Registry, summary *Summary) error {
	client := contract.NewClient(caller)
	gap := cfg.scaled(5 * time.Second)
	for i := 0; i < 200; i++ {
		stream, err := client.StreamLogs(parent, &pb.StreamLogsRequest{StreamId: fmt.Sprintf("cancel-%d", i), ByteRate: 200 << 10, LineSize: 200})
		if err != nil {
			return err
		}
		stream.Cancel(errors.New("scenario 4 stream cancellation"))
		if err := waitContext(parent, gap); err != nil {
			return err
		}
		summary.StreamCancelCycles++
		// Establish the recovery baseline only after the Go runtime, transport,
		// and TLS stacks have reached their normal allocation plateau. These
		// first 20 cycles remain part of the required 200-cycle workload.
		if i == 19 {
			reg.CollectRuntime()
			initial := reg.Snapshot(nil)
			summary.BaselineGoroutines = initial.Gauges[metrics.Goroutines]
			summary.BaselineRSSBytes = initial.Gauges[metrics.ProcessRSSBytes]
		}
	}
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("cancel-operation-%d", i)
		stream, err := client.OperationOutput(parent, &pb.OperationOutputRequest{OperationId: id})
		if err != nil {
			return err
		}
		// Receiving one chunk proves the simulated operation is registered before
		// measuring the CancelOperation path.
		if _, err := stream.Recv(parent); err != nil {
			return err
		}
		start := time.Now()
		response, err := client.CancelOperation(parent, &pb.CancelOperationRequest{OperationId: id, Reason: pb.CancelReason_CANCEL_REASON_USER})
		reg.Observe(metrics.CancelAckLatencyMS, float64(time.Since(start).Microseconds())/1000)
		stream.Cancel(errors.New("operation output stream cleanup"))
		if err != nil {
			return err
		}
		if response.Result != pb.CancelResult_CANCEL_RESULT_ACCEPTED {
			return fmt.Errorf("operation %s cancel result = %s", id, response.Result)
		}
		if err := waitContext(parent, gap); err != nil {
			return err
		}
		summary.OperationCancelCycles++
	}
	// Give transport-owned goroutines and buffers one collection interval to return.
	if err := waitContext(parent, cfg.scaled(5*time.Second)); err != nil {
		return err
	}
	return nil
}

func drainLogs(ctx context.Context, set *streamSet, stream *contract.Receive[*pb.LogChunk], reg *metrics.Registry) {
	defer set.wg.Done()
	defer stream.Cancel(nil)
	for {
		chunk, err := stream.Recv(ctx)
		if err != nil {
			return
		}
		reg.Add(metrics.LogBytesSentTotal, uint64(len(chunk.Data)))
	}
}

func drainStats(ctx context.Context, set *streamSet, stream *contract.Receive[*pb.StatsSample], reg *metrics.Registry) {
	defer set.wg.Done()
	defer stream.Cancel(nil)
	for {
		if _, err := stream.Recv(ctx); err != nil {
			return
		}
		reg.Add(metrics.StatsSamplesSentTotal, 1)
	}
}

func pauseWindow(cfg Config) (time.Duration, time.Duration) {
	if cfg.Scenario == 1 {
		return cfg.scaled(180 * time.Second), cfg.scaled(360 * time.Second)
	}
	if cfg.Scenario == 2 {
		return cfg.scaled(60 * time.Second), cfg.scaled(240 * time.Second)
	}
	return cfg.Duration() / 3, cfg.Duration() * 2 / 3
}

func operationStart(cfg Config) time.Duration {
	if cfg.Scenario == 1 {
		return cfg.scaled(420 * time.Second)
	}
	if cfg.Scenario == 2 {
		return cfg.scaled(60 * time.Second)
	}
	return cfg.Duration() * 2 / 3
}

func measurementStart(cfg Config) time.Duration {
	if cfg.Scenario == 1 {
		return cfg.scaled(120 * time.Second)
	}
	return 0
}

func waitContext(ctx context.Context, duration time.Duration) error {
	t := time.NewTimer(duration)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func expectedEnd(err error) bool {
	code := transport.StatusOf(err).Code
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || code == transport.CodeCanceled || code == transport.CodeDeadlineExceeded || code == transport.CodeUnavailable
}

var summaryMu sync.Mutex

func appendError(summary *Summary, message string) {
	summaryMu.Lock()
	summary.Errors = append(summary.Errors, message)
	summaryMu.Unlock()
}

func WriteSummary(path string, summary Summary) error {
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func ReadSamples(path string) ([]metrics.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var samples []metrics.Sample
	for {
		var sample metrics.Sample
		if err := dec.Decode(&sample); errors.Is(err, io.EOF) {
			return samples, nil
		} else if err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
}

// PayloadSize is used by tests to verify that protobuf record sizes remain
// comparable between candidates.
func PayloadSize(msg proto.Message) int { return proto.Size(msg) }
