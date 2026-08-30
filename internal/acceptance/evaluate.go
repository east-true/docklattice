// Package acceptance evaluates Appendix A.9 without weakening thresholds for
// a failing candidate. Missing evidence is a failure, not an implicit pass.
package acceptance

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/east-true/docklattice/internal/experiment"
	"github.com/east-true/docklattice/internal/metrics"
)

type Check struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Passed   bool    `json:"passed"`
	Observed string  `json:"observed"`
	Limit    string  `json:"limit"`
	Value    float64 `json:"value,omitempty"`
}

type Report struct {
	Candidate      string  `json:"candidate"`
	Scenario       int     `json:"scenario"`
	Network        string  `json:"network"`
	Trial          int     `json:"trial"`
	OfficialTiming bool    `json:"official_timing"`
	Passed         bool    `json:"passed"`
	Checks         []Check `json:"checks"`
}

type Evidence struct {
	Summary        experiment.Summary
	Server         []metrics.Sample
	Agents         [][]metrics.Sample
	Baseline       *experiment.Summary
	BaselineServer []metrics.Sample
	NetworkControl string
	RequireExact   bool
}

func Load(dir string) (Evidence, error) {
	var e Evidence
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e.Summary); err != nil {
		return e, err
	}
	e.Server, err = experiment.ReadSamples(filepath.Join(dir, "server.jsonl"))
	if err != nil {
		return e, err
	}
	agentPaths, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return e, err
	}
	sort.Strings(agentPaths)
	for _, path := range agentPaths {
		samples, err := experiment.ReadSamples(path)
		if err != nil {
			return e, err
		}
		e.Agents = append(e.Agents, samples)
	}
	controlPaths, err := filepath.Glob(filepath.Join(dir, "network-control*.txt"))
	if err != nil {
		return e, err
	}
	for _, path := range controlPaths {
		b, err := os.ReadFile(path)
		if err != nil {
			return e, err
		}
		e.NetworkControl += string(b) + "\n"
	}
	return e, nil
}

func Evaluate(e Evidence) Report {
	r := Report{Candidate: e.Summary.Candidate, Scenario: e.Summary.Scenario, Network: e.Summary.Network, Trial: e.Summary.Trial, OfficialTiming: e.Summary.OfficialTiming}
	add := func(check Check) { r.Checks = append(r.Checks, check) }
	if e.RequireExact {
		evaluateControls(e, add)
	}
	expectedAgents := e.Summary.ExpectedAgents
	configOK := validScenarioConfig(e.Summary.Config)
	ratesOK, ratesObserved := workloadRates(e)
	workloadOK := configOK && ratesOK && expectedAgents > 0 && len(e.Agents) == expectedAgents && len(e.Server) > 0 && len(e.Summary.Errors) == 0 && e.Summary.Registrations == expectedAgents && e.Summary.CoverageSnapshots == expectedAgents && e.Summary.Heartbeats == expectedAgents
	add(Check{ID: "workload", Name: "workload and logical-contract integrity", Passed: workloadOK, Observed: fmt.Sprintf("config=%t agents=%d/%d samples=%d errors=%d register/coverage/heartbeat=%d/%d/%d; %s", configOK, len(e.Agents), expectedAgents, len(e.Server), len(e.Summary.Errors), e.Summary.Registrations, e.Summary.CoverageSnapshots, e.Summary.Heartbeats, ratesObserved), Limit: "exact scenario config; all processes sampled; requested load >=98%; no errors; bootstrap once per Agent"})
	switch e.Summary.Scenario {
	case 1, 2:
		evaluateInterference(e, add)
	case 3:
		evaluateMemory(e, add, 512<<20, 256<<20)
		if e.Summary.ExpectedAgents > 1 {
			evaluateScaleIncrement(e, add)
		}
	case 4:
		evaluateLeakAndCancel(e, add)
	default:
		add(Check{ID: "scenario", Name: "known scenario", Passed: false, Observed: fmt.Sprint(e.Summary.Scenario), Limit: "1..4"})
	}
	r.Passed = len(r.Checks) > 0
	for _, check := range r.Checks {
		r.Passed = r.Passed && check.Passed
	}
	return r
}

func validScenarioConfig(c experiment.Config) bool {
	common := c.TimeScale == 1 && c.ControlledHarness && c.AuditPayloadBytes == 512 && c.AuditMode == "managed-like" && c.EchoPayload == 1024 && c.LogBytesSecond == 200<<10 && c.LogLineBytes == 200
	if !common {
		return false
	}
	switch c.Scenario {
	case 1:
		return c.Agents == 1 && c.AuditRate == 20 && c.StatsTargets == 6 && c.LogStreams == 4
	case 2:
		return c.Agents == 1 && c.PauseLog && (c.AuditRate == 20 || c.AuditRate == 50 || c.AuditRate == 100) && c.StatsTargets == 6 && c.LogStreams == 4
	case 3:
		return !c.PauseLog && (c.Agents == 1 || c.Agents == 20) && c.AuditRate == 5 && c.StatsTargets == 1 && c.LogStreams == 1
	case 4:
		return !c.PauseLog && c.Agents == 1 && c.AuditRate == 0 && c.StatsTargets == 0 && c.LogStreams == 0
	default:
		return false
	}
}

func workloadRates(e Evidence) (bool, string) {
	cfg := e.Summary.Config
	duration := e.Summary.WorkloadElapsedSeconds
	expectedDuration := cfg.Duration().Seconds()
	durationOK := expectedDuration > 0 && duration >= expectedDuration*.98
	minAuditRate := math.MaxFloat64
	auditOK := cfg.AuditRate == 0
	if cfg.AuditRate > 0 {
		auditOK = len(e.Agents) == cfg.Agents && len(e.Agents) > 0
		for _, samples := range e.Agents {
			samples = samplesUntilElapsed(samples, duration)
			rate, ok := sampleCounterRate(samples, metrics.AuditGeneratedTotal)
			auditOK = auditOK && ok && rate >= float64(cfg.AuditRate)*.98
			minAuditRate = math.Min(minAuditRate, rate)
		}
	}
	if minAuditRate == math.MaxFloat64 {
		minAuditRate = 0
	}
	logsOK := true
	minLogRate := math.MaxFloat64
	if cfg.Scenario == 1 || cfg.Scenario == 2 {
		for i := 2; i <= cfg.LogStreams; i++ {
			key := fmt.Sprintf("log-%d", i)
			rate := float64(e.Summary.LogBytesBeforePause[key]) / e.Summary.LogBeforeSeconds
			minLogRate = math.Min(minLogRate, rate)
			logsOK = logsOK && rate >= float64(cfg.LogBytesSecond)*.98
		}
	} else if cfg.Scenario == 3 {
		rate, ok := sampleCounterRate(samplesUntilElapsed(e.Server, duration), metrics.LogBytesSentTotal)
		minLogRate = rate / float64(max(1, cfg.Agents))
		logsOK = ok && minLogRate >= float64(cfg.LogBytesSecond)*.98
	}
	if minLogRate == math.MaxFloat64 {
		minLogRate = 0
	}
	statsOK := true
	statsRate := 0.0
	echoOK := true
	echoRate := 0.0
	if cfg.Scenario == 1 || cfg.Scenario == 2 {
		statsRate = float64(e.Summary.StatsSamples) / duration
		echoRate = float64(e.Summary.EchoRequests) / duration
		statsOK = statsRate >= float64(cfg.StatsTargets)/2*.98
		echoOK = echoRate >= .5*.98
	} else if cfg.Scenario == 3 {
		var ok bool
		statsRate, ok = sampleCounterRate(e.Server, metrics.StatsSamplesSentTotal)
		statsRate /= float64(max(1, cfg.Agents))
		statsOK = ok && statsRate >= .5*.98
	}
	return durationOK && auditOK && logsOK && statsOK && echoOK, fmt.Sprintf("duration=%.1f/%.1fs min_audit=%.2f/s min_log=%.1fB/s stats=%.3f/s echo=%.3f/s", duration, expectedDuration, minAuditRate, minLogRate, statsRate, echoRate)
}

func sampleCounterRate(samples []metrics.Sample, name string) (float64, bool) {
	if len(samples) < 2 {
		return 0, false
	}
	seconds := sampleDuration(samples)
	if seconds <= 0 {
		return 0, false
	}
	return float64(counterDeltaUint(samples, name)) / seconds, true
}

func evaluateScaleIncrement(e Evidence, add func(Check)) {
	currentAgents := e.Summary.ExpectedAgents
	baselineAgents := 0
	if e.Baseline != nil {
		baselineAgents = e.Baseline.ExpectedAgents
	}
	currentRSS := maxGauge(e.Server, metrics.ProcessRSSBytes)
	baselineRSS := maxGauge(e.BaselineServer, metrics.ProcessRSSBytes)
	deltaAgents := currentAgents - baselineAgents
	ok := e.Baseline != nil && baselineAgents == 1 && currentAgents == 20 && len(e.BaselineServer) > 0 && currentRSS > 0 && baselineRSS > 0 && deltaAgents > 0
	perAgent := 0.0
	if ok {
		perAgent = (currentRSS - baselineRSS) / float64(deltaAgents)
	}
	add(Check{ID: "diag.server.incremental_rss", Name: "server incremental RSS per Agent", Passed: ok, Observed: fmt.Sprintf("%.3f MiB/Agent from %.1fMiB@%d to %.1fMiB@%d", perAgent/(1<<20), baselineRSS/(1<<20), baselineAgents, currentRSS/(1<<20), currentAgents), Limit: "matched 1-Agent baseline and 20-Agent scale evidence required; no A.9 per-Agent threshold", Value: perAgent})
}

func evaluateControls(e Evidence, add func(Check)) {
	const (
		serverMemoryMax = 1 << 30
		agentMemoryMax  = 512 << 20
	)
	serverOK := lastGaugeEquals(e.Server, metrics.CgroupMemoryMaxBytes, serverMemoryMax) &&
		lastGaugeEquals(e.Server, metrics.CgroupMemoryHighBytes, 0) &&
		lastGaugeEquals(e.Server, metrics.CgroupMemorySwapMaxBytes, 0) &&
		cpuBoundToOne(e.Server) &&
		lastGaugeEquals(e.Server, metrics.GoMaxProcs, 1) &&
		lastGaugeEquals(e.Server, metrics.GoGCTargetPercent, 100) &&
		lastGaugeEquals(e.Server, metrics.RlimitNoFileSoft, 4096) &&
		lastGaugeEquals(e.Server, metrics.RlimitNoFileHard, 4096)
	agentsOK := len(e.Agents) == e.Summary.ExpectedAgents && len(e.Agents) > 0
	for _, samples := range e.Agents {
		agentsOK = agentsOK && lastGaugeEquals(samples, metrics.CgroupMemoryMaxBytes, agentMemoryMax) &&
			lastGaugeEquals(samples, metrics.CgroupMemoryHighBytes, 0) &&
			lastGaugeEquals(samples, metrics.CgroupMemorySwapMaxBytes, 0) &&
			cpuBoundToOne(samples) &&
			lastGaugeEquals(samples, metrics.GoMaxProcs, 1) &&
			lastGaugeEquals(samples, metrics.GoGCTargetPercent, 100) &&
			lastGaugeEquals(samples, metrics.RlimitNoFileSoft, 4096) &&
			lastGaugeEquals(samples, metrics.RlimitNoFileHard, 4096)
	}
	monotonicOK := monotonicAxis(e.Server)
	for _, samples := range e.Agents {
		monotonicOK = monotonicOK && monotonicAxis(samples)
	}
	add(Check{ID: "protocol.clock", Name: "monotonic sampling axis", Passed: monotonicOK, Observed: fmt.Sprintf("server_samples=%d agent_series=%d", len(e.Server), len(e.Agents)), Limit: "strictly increasing elapsed_seconds on every process; wall clock is correlation-only"})
	networkOK := false
	if e.Summary.Network == "loopback" {
		networkOK = strings.Contains(e.NetworkControl, "loopback without netem")
	} else if e.Summary.Network == "netem" {
		networkOK = strings.Contains(e.NetworkControl, "qdisc netem") && strings.Contains(e.NetworkControl, "delay 10ms") && strings.Contains(e.NetworkControl, "loss 1%")
	}
	passed := e.Summary.OfficialTiming && e.Summary.ControlledHarness && serverOK && agentsOK && networkOK
	add(Check{ID: "protocol", Name: "official timing and controls", Passed: passed, Observed: fmt.Sprintf("official=%t controlled=%t server=%t agents=%t network=%t", e.Summary.OfficialTiming, e.Summary.ControlledHarness, serverOK, agentsOK, networkOK), Limit: "time_scale=1; memory.max=1GiB/512MiB; memory.high=max; swap=0; CPU quota or affinity=1; GOMAXPROCS=1; GOGC=100; nofile=4096; loopback or 10ms/1% netem proof"})
}

func monotonicAxis(samples []metrics.Sample) bool {
	if len(samples) < 2 || samples[0].ElapsedSeconds <= 0 {
		return false
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].ElapsedSeconds <= samples[i-1].ElapsedSeconds {
			return false
		}
	}
	return true
}

func evaluateInterference(e Evidence, add func(Check)) {
	delta := percentDelta(e.Summary.OperationDurationMS, e.Summary.OperationExpectedMS)
	if e.Baseline != nil && e.Baseline.OperationDurationMS > 0 {
		delta = percentDelta(e.Summary.OperationDurationMS, e.Baseline.OperationDurationMS)
	}
	add(Check{ID: "1", Name: "operation delay", Passed: e.Summary.OperationDurationMS > 0 && delta <= 5, Observed: fmt.Sprintf("%.2f%% delta (%.1fms)", delta, e.Summary.OperationDurationMS), Limit: "<=5%", Value: delta})

	final := last(e.Server)
	for _, quantile := range []string{"P50", "P95", "P99"} {
		value, ok := percentile(final, metrics.QueryEchoRTTMS, quantile)
		add(Check{ID: "diag.echo." + strings.ToLower(quantile), Name: "query Echo RTT " + strings.ToLower(quantile), Passed: ok, Observed: observed(value, ok, "ms"), Limit: "diagnostic evidence required; no A.9 threshold", Value: value})
	}
	cancelP99, cancelOK := percentile(final, metrics.CancelAckLatencyMS, "P99")
	progressP99, progressOK := percentile(final, metrics.OperationProgressEventLatencyMS, "P99")
	// Scenario 1 does not itself cancel an operation; scenario 4 supplies the
	// cancel distribution. Mark missing evidence explicitly rather than pass.
	add(Check{ID: "2a", Name: "cancel ACK latency", Passed: cancelOK && cancelP99 <= 500, Observed: observed(cancelP99, cancelOK, "ms p99"), Limit: "<=500ms", Value: cancelP99})
	add(Check{ID: "2b", Name: "operation progress latency", Passed: progressOK && progressP99 <= 1000, Observed: observed(progressP99, progressOK, "ms p99"), Limit: "<=1000ms", Value: progressP99})

	cursorAdvances := e.Summary.AuditAdvancesInPause > 0 && e.Summary.AuditCursorPauseEnd > e.Summary.AuditCursorPauseStart
	add(Check{ID: "3a", Name: "audit cursor advances", Passed: cursorAdvances, Observed: fmt.Sprintf("%d advances (%d→%d) during stalled-log interval", e.Summary.AuditAdvancesInPause, e.Summary.AuditCursorPauseStart, e.Summary.AuditCursorPauseEnd), Limit: ">0 throughout stalled-log interval", Value: float64(e.Summary.AuditAdvancesInPause)})
	add(Check{ID: "3b", Name: "audit ACK stall", Passed: e.Summary.MaxAuditStallSeconds <= 10 && e.Summary.MaxAuditStallSeconds > 0, Observed: fmt.Sprintf("%.3fs max", e.Summary.MaxAuditStallSeconds), Limit: "<=10s", Value: e.Summary.MaxAuditStallSeconds})

	generatedRate, syncedRate, rateOK := auditRates(e)
	add(Check{ID: "4a", Name: "audit throughput", Passed: rateOK && syncedRate >= generatedRate, Observed: fmt.Sprintf("generated=%.2f/s synced=%.2f/s", generatedRate, syncedRate), Limit: "synced >= generated", Value: syncedRate - generatedRate})
	measurementAgents := make([][]metrics.Sample, 0, len(e.Agents))
	for _, samples := range e.Agents {
		measurementAgents = append(measurementAgents, samplesFromElapsed(samples, e.Summary.MeasurementStartedElapsedSeconds))
	}
	lagSlope, slopeOK := maxAgentGaugeSlope(measurementAgents, metrics.AuditSyncLagRecords)
	persistentLag := anyPersistentIncrease(measurementAgents, metrics.AuditSyncLagRecords)
	add(Check{ID: "4b", Name: "audit lag slope", Passed: slopeOK && !persistentLag, Observed: observed(lagSlope, slopeOK, "records/s"), Limit: "not persistently positive", Value: lagSlope})

	stats := e.Summary.StatsSamples
	add(Check{ID: "5", Name: "stats latest-wins", Passed: stats > 0, Observed: fmt.Sprintf("%d samples; bounded single-slot producer", stats), Limit: "samples delivered, no backlog", Value: float64(stats)})

	for i := 2; i <= len(e.Summary.LogBytesByStream); i++ {
		key := fmt.Sprintf("log-%d", i)
		before := float64(e.Summary.LogBytesBeforePause[key]) / e.Summary.LogBeforeSeconds
		during := float64(e.Summary.LogBytesDuringPause[key]) / e.Summary.LogPauseSeconds
		d := percentDelta(during, before)
		add(Check{ID: fmt.Sprintf("6.%d", i), Name: key + " isolation", Passed: before > 0 && d <= 10, Observed: fmt.Sprintf("%.2f%% throughput delta", d), Limit: "<=10%", Value: d})
	}

	evaluateMemory(e, add, 512<<20, 256<<20)
}

func evaluateMemory(e Evidence, add func(Check), serverLimit, agentLimit float64) {
	serverRSS := maxGauge(e.Server, metrics.ProcessRSSBytes)
	serverOOM := maxGauge(e.Server, metrics.CgroupMemoryEventsOOMTotal) + maxGauge(e.Server, metrics.CgroupMemoryEventsOOMKillTotal)
	add(Check{ID: "7.server.rss", Name: "server RSS", Passed: serverRSS > 0 && serverRSS <= serverLimit, Observed: humanBytes(serverRSS), Limit: "<= " + humanBytes(serverLimit), Value: serverRSS})
	add(Check{ID: "7.server.oom", Name: "server cgroup OOM", Passed: serverOOM == 0, Observed: fmt.Sprintf("%.0f events", serverOOM), Limit: "0", Value: serverOOM})
	maxAgentRSS := 0.0
	agentOOM := 0.0
	for _, samples := range e.Agents {
		maxAgentRSS = math.Max(maxAgentRSS, maxGauge(samples, metrics.ProcessRSSBytes))
		agentOOM += maxGauge(samples, metrics.CgroupMemoryEventsOOMTotal) + maxGauge(samples, metrics.CgroupMemoryEventsOOMKillTotal)
	}
	add(Check{ID: "7.agent.rss", Name: "agent RSS", Passed: len(e.Agents) > 0 && maxAgentRSS <= agentLimit, Observed: humanBytes(maxAgentRSS), Limit: "<= " + humanBytes(agentLimit), Value: maxAgentRSS})
	add(Check{ID: "7.agent.oom", Name: "agent cgroup OOM", Passed: len(e.Agents) > 0 && agentOOM == 0, Observed: fmt.Sprintf("%.0f events", agentOOM), Limit: "0", Value: agentOOM})
	for _, metric := range []struct {
		id, name string
		minimum  float64
	}{
		{"rss", "process RSS", 1 << 20},
		{"heap", "Go heap after GC cycles", 1 << 20},
		{"anon", "cgroup anon memory", 1 << 20},
		{"buffer", "bounded buffer memory", 64 << 10},
	} {
		metricName := map[string]string{"rss": metrics.ProcessRSSBytes, "heap": metrics.GoHeapAllocBytes, "anon": metrics.CgroupMemoryAnonBytes, "buffer": metrics.BufferBytes}[metric.id]
		serverSamples := e.Server
		if metric.id == "heap" {
			serverSamples = postGCSamples(serverSamples)
		}
		serverGrowth, serverLeak, serverOK := sustainedGrowth(serverSamples, metricName, metric.minimum)
		add(Check{ID: "7.server." + metric.id + ".trend", Name: "server " + metric.name + " trend", Passed: serverOK && !serverLeak, Observed: observed(serverGrowth, serverOK, "% growth across window medians"), Limit: "no sustained monotonic growth >20% and threshold", Value: serverGrowth})
		maxGrowth := 0.0
		agentLeak := false
		agentOK := len(e.Agents) > 0
		for _, samples := range e.Agents {
			if metric.id == "heap" {
				samples = postGCSamples(samples)
			}
			growth, leak, ok := sustainedGrowth(samples, metricName, metric.minimum)
			agentOK = agentOK && ok
			maxGrowth = math.Max(maxGrowth, growth)
			agentLeak = agentLeak || leak
		}
		add(Check{ID: "7.agent." + metric.id + ".trend", Name: "agent " + metric.name + " trend", Passed: agentOK && !agentLeak, Observed: observed(maxGrowth, agentOK, "% maximum growth"), Limit: "no sustained monotonic growth >20% and threshold", Value: maxGrowth})
	}
	if e.Summary.Candidate == "websocket" {
		const name = "websocket_buffer_bytes"
		serverGrowth, serverLeak, serverOK := sustainedGrowth(e.Server, name, 64<<10)
		add(Check{ID: "7.server.websocket_buffer.trend", Name: "server WebSocket transport-buffer trend", Passed: serverOK && !serverLeak, Observed: observed(serverGrowth, serverOK, "% growth across window medians"), Limit: "no sustained monotonic growth >20% and 64KiB", Value: serverGrowth})
		maxGrowth := 0.0
		agentLeak := false
		agentOK := len(e.Agents) > 0
		for _, samples := range e.Agents {
			growth, leak, ok := sustainedGrowth(samples, name, 64<<10)
			agentOK = agentOK && ok
			maxGrowth = math.Max(maxGrowth, growth)
			agentLeak = agentLeak || leak
		}
		add(Check{ID: "7.agent.websocket_buffer.trend", Name: "agent WebSocket transport-buffer trend", Passed: agentOK && !agentLeak, Observed: observed(maxGrowth, agentOK, "% maximum growth"), Limit: "no sustained monotonic growth >20% and 64KiB", Value: maxGrowth})
	}
}

func evaluateLeakAndCancel(e Evidence, add func(Check)) {
	goroutineRatio := ratio(e.Summary.FinalGoroutines, e.Summary.BaselineGoroutines)
	rssRatio := ratio(e.Summary.FinalRSSBytes, e.Summary.BaselineRSSBytes)
	add(Check{ID: "8a", Name: "goroutine recovery", Passed: e.Summary.StreamCancelCycles == 200 && e.Summary.OperationCancelCycles == 50 && goroutineRatio <= 1.05, Observed: fmt.Sprintf("%.3fx after %d/%d cycles", goroutineRatio, e.Summary.StreamCancelCycles, e.Summary.OperationCancelCycles), Limit: "<=1.05x after 200 stream + 50 operation cycles", Value: goroutineRatio})
	add(Check{ID: "8b", Name: "RSS recovery", Passed: rssRatio <= 1.20, Observed: fmt.Sprintf("%.3fx", rssRatio), Limit: "<=1.20x", Value: rssRatio})
	maxBuffer := 0.0
	serverFinal := last(e.Server)
	serverBuffer, serverBufferOK := serverFinal.Gauges[metrics.BufferBytes]
	maxBuffer = math.Max(maxBuffer, serverBuffer)
	bufferRecovered := serverBufferOK && serverBuffer <= 64<<10 && len(e.Agents) > 0
	for _, samples := range e.Agents {
		if len(samples) == 0 {
			bufferRecovered = false
			continue
		}
		final := samples[len(samples)-1].Gauges[metrics.BufferBytes]
		maxBuffer = math.Max(maxBuffer, final)
		bufferRecovered = bufferRecovered && final <= 64<<10
	}
	add(Check{ID: "8.buffer", Name: "buffer recovery", Passed: bufferRecovered, Observed: humanBytes(maxBuffer), Limit: "<=64KiB after stream termination", Value: maxBuffer})
	finalGauges := serverFinal.Gauges
	if e.Summary.Candidate == "grpc" {
		active, activeOK := finalGauges["grpc_active_exchanges"]
		local, localOK := finalGauges["grpc_local_flow_control_window_recovery_peak_bytes"]
		remote, remoteOK := finalGauges["grpc_remote_flow_control_window_recovery_peak_bytes"]
		windowOK := localOK && remoteOK && percentDelta(local, 64<<10) <= 5 && percentDelta(remote, 64<<10) <= 5
		add(Check{ID: "8c", Name: "gRPC stream/window recovery", Passed: activeOK && active == 0 && windowOK, Observed: fmt.Sprintf("active=%.0f local=%.0f remote=%.0f", active, local, remote), Limit: "active=0; windows within 5% of 65536"})
	} else if e.Summary.Candidate == "websocket" {
		serverOK, serverObserved := websocketRecovered(finalGauges)
		agentsOK := len(e.Agents) > 0
		agentObserved := ""
		for i, samples := range e.Agents {
			ok, observed := websocketRecovered(last(samples).Gauges)
			agentsOK = agentsOK && ok
			if i == 0 {
				agentObserved = observed
			}
		}
		add(Check{ID: "8c", Name: "WebSocket stream/credit recovery", Passed: serverOK && agentsOK, Observed: "server[" + serverObserved + "] agent_max[" + agentObserved + "]", Limit: "both endpoints: active=0; credit=0; receive buffer=0; all class queues=0"})
	}
	final := last(e.Server)
	p99, ok := percentile(final, metrics.CancelAckLatencyMS, "P99")
	add(Check{ID: "2a", Name: "cancel ACK latency", Passed: ok && p99 <= 500, Observed: observed(p99, ok, "ms p99"), Limit: "<=500ms", Value: p99})
	evaluateMemory(e, add, 512<<20, 256<<20)
}

func WriteJSON(path string, report Report) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func WriteMarkdown(path string, reports []Report) error {
	var b strings.Builder
	b.WriteString("# Transport Prototype acceptance report\n\n")
	b.WriteString("| Candidate | Network | Scenario | Trial | Official | Result |\n|---|---|---:|---:|---|---|\n")
	for _, report := range reports {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %t | %s |\n", report.Candidate, report.Network, report.Scenario, report.Trial, report.OfficialTiming, passWord(report.Passed))
	}
	for _, report := range reports {
		fmt.Fprintf(&b, "\n## %s / %s / scenario %d / trial %d\n\n", report.Candidate, report.Network, report.Scenario, report.Trial)
		b.WriteString("| ID | Check | Result | Observed | Limit |\n|---|---|---|---|---|\n")
		for _, check := range report.Checks {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", check.ID, check.Name, passWord(check.Passed), check.Observed, check.Limit)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func last(samples []metrics.Sample) metrics.Sample {
	if len(samples) == 0 {
		return metrics.Sample{}
	}
	return samples[len(samples)-1]
}

func percentile(sample metrics.Sample, name, field string) (float64, bool) {
	d, ok := sample.Distributions[name]
	if !ok || d.Count == 0 {
		return 0, false
	}
	switch field {
	case "P50":
		return d.P50, true
	case "P95":
		return d.P95, true
	case "P99":
		return d.P99, true
	default:
		return 0, false
	}
}

func auditRates(e Evidence) (float64, float64, bool) {
	serverSamples := samplesUntilElapsed(samplesFromElapsed(e.Server, e.Summary.MeasurementStartedElapsedSeconds), e.Summary.WorkloadElapsedSeconds)
	if len(serverSamples) < 2 || len(e.Agents) == 0 {
		return 0, 0, false
	}
	serverSeconds := sampleDuration(serverSamples)
	if serverSeconds <= 0 {
		return 0, 0, false
	}
	generated := 0.0
	for _, samples := range e.Agents {
		samples = samplesUntilElapsed(samplesFromElapsed(samples, e.Summary.MeasurementStartedElapsedSeconds), e.Summary.WorkloadElapsedSeconds)
		if len(samples) < 2 {
			return 0, 0, false
		}
		seconds := sampleDuration(samples)
		if seconds <= 0 {
			return 0, 0, false
		}
		generated += float64(counterDeltaUint(samples, metrics.AuditGeneratedTotal)) / seconds
	}
	synced := float64(counterDeltaUint(serverSamples, metrics.AuditSyncedTotal)) / serverSeconds
	// One-second samplers on separate processes are not phase-locked. Report and
	// compare at the metric's displayed 0.01 record/s resolution so sub-millisecond
	// scrape jitter cannot turn equal integer progress into a false deficit.
	return math.Round(generated*100) / 100, math.Round(synced*100) / 100, true
}

func samplesFromElapsed(samples []metrics.Sample, start float64) []metrics.Sample {
	if start <= 0 {
		return samples
	}
	idx := sort.Search(len(samples), func(i int) bool { return samples[i].ElapsedSeconds >= start })
	return samples[idx:]
}

func samplesUntilElapsed(samples []metrics.Sample, end float64) []metrics.Sample {
	if end <= 0 {
		return samples
	}
	idx := sort.Search(len(samples), func(i int) bool { return samples[i].ElapsedSeconds > end })
	return samples[:idx]
}

func sampleDuration(samples []metrics.Sample) float64 {
	if len(samples) < 2 {
		return 0
	}
	first, last := samples[0], samples[len(samples)-1]
	if first.ElapsedSeconds > 0 && last.ElapsedSeconds > first.ElapsedSeconds {
		return last.ElapsedSeconds - first.ElapsedSeconds
	}
	// Backward-compatible fallback for unit fixtures and pre-fix diagnostics.
	return last.At.Sub(first.At).Seconds()
}

func maxAgentGaugeSlope(agents [][]metrics.Sample, name string) (float64, bool) {
	maxSlope := math.Inf(-1)
	found := false
	for _, samples := range agents {
		if len(samples) < 2 {
			continue
		}
		start := (len(samples) - 1) / 2
		dt := sampleDuration(samples[start:])
		if dt <= 0 {
			continue
		}
		slope := (samples[len(samples)-1].Gauges[name] - samples[start].Gauges[name]) / dt
		maxSlope = math.Max(maxSlope, slope)
		found = true
	}
	if !found {
		return 0, false
	}
	return maxSlope, found
}

func anyPersistentIncrease(groups [][]metrics.Sample, name string) bool {
	for _, samples := range groups {
		if len(samples) < 10 {
			return true
		}
		medians := make([]float64, 0, 5)
		for window := 0; window < 5; window++ {
			start := window * len(samples) / 5
			end := (window + 1) * len(samples) / 5
			values := make([]float64, 0, end-start)
			for _, sample := range samples[start:end] {
				values = append(values, sample.Gauges[name])
			}
			sort.Float64s(values)
			medians = append(medians, median(values))
		}
		increasing := true
		for i := 1; i < len(medians); i++ {
			if medians[i] <= medians[i-1] {
				increasing = false
				break
			}
		}
		if increasing {
			return true
		}
	}
	return false
}

func maxGauge(samples []metrics.Sample, name string) float64 {
	maxValue := 0.0
	for _, sample := range samples {
		maxValue = math.Max(maxValue, sample.Gauges[name])
	}
	return maxValue
}

func lastGaugeEquals(samples []metrics.Sample, name string, want float64) bool {
	if len(samples) == 0 {
		return false
	}
	got, ok := samples[len(samples)-1].Gauges[name]
	return ok && got == want
}

func cpuBoundToOne(samples []metrics.Sample) bool {
	return lastGaugeEquals(samples, metrics.CgroupCPUQuotaCores, 1) || lastGaugeEquals(samples, metrics.ProcessCPUAffinityCores, 1)
}

func websocketRecovered(gauges map[string]float64) (bool, string) {
	active, activeOK := gauges["websocket_active_streams"]
	creditBytes, creditBytesOK := gauges["websocket_credit_bytes_available"]
	creditMessages, creditMessagesOK := gauges["websocket_credit_messages_available"]
	creditBytesMin, creditBytesMinOK := gauges["websocket_stream_credit_bytes_min"]
	creditBytesMax, creditBytesMaxOK := gauges["websocket_stream_credit_bytes_max"]
	creditMessagesMin, creditMessagesMinOK := gauges["websocket_stream_credit_messages_min"]
	creditMessagesMax, creditMessagesMaxOK := gauges["websocket_stream_credit_messages_max"]
	buffer, bufferOK := gauges["websocket_receive_buffer_bytes"]
	queuedBytes, queuedBytesOK := gauges["websocket_send_queue_bytes"]
	queues := 0.0
	queuesOK := true
	for i := 0; i < 5; i++ {
		value, exists := gauges[fmt.Sprintf("websocket_send_queue_p%d_frames", i)]
		queues += value
		queuesOK = queuesOK && exists
	}
	creditExtremaOK := creditBytesMinOK && creditBytesMaxOK && creditMessagesMinOK && creditMessagesMaxOK
	ok := activeOK && creditBytesOK && creditMessagesOK && creditExtremaOK && bufferOK && queuedBytesOK && queuesOK && active == 0 && creditBytes == 0 && creditMessages == 0 && creditBytesMin == 0 && creditBytesMax == 0 && creditMessagesMin == 0 && creditMessagesMax == 0 && buffer == 0 && queuedBytes == 0 && queues == 0
	return ok, fmt.Sprintf("active=%.0f credit_bytes=%.0f credit_messages=%.0f credit_bytes_min/max=%.0f/%.0f credit_messages_min/max=%.0f/%.0f receive_buffer=%.0f send_queue_bytes=%.0f queued=%.0f", active, creditBytes, creditMessages, creditBytesMin, creditBytesMax, creditMessagesMin, creditMessagesMax, buffer, queuedBytes, queues)
}

func sustainedGrowth(samples []metrics.Sample, name string, absoluteMinimum float64) (float64, bool, bool) {
	if len(samples) < 10 {
		return 0, false, false
	}
	for _, sample := range samples {
		if _, ok := sample.Gauges[name]; !ok {
			return 0, false, false
		}
	}
	const windows = 5
	medians := make([]float64, 0, windows)
	for window := 0; window < windows; window++ {
		start := window * len(samples) / windows
		end := (window + 1) * len(samples) / windows
		if end <= start {
			return 0, false, false
		}
		values := make([]float64, 0, end-start)
		for _, sample := range samples[start:end] {
			values = append(values, sample.Gauges[name])
		}
		sort.Float64s(values)
		medians = append(medians, median(values))
	}
	monotonic := true
	for i := 1; i < len(medians); i++ {
		if medians[i] <= medians[i-1] {
			monotonic = false
			break
		}
	}
	growth := signedPercentChange(medians[len(medians)-1], medians[0])
	absolute := medians[len(medians)-1] - medians[0]
	leak := monotonic && growth > 20 && absolute > absoluteMinimum
	return growth, leak, true
}

func postGCSamples(samples []metrics.Sample) []metrics.Sample {
	if len(samples) < 2 {
		return nil
	}
	out := make([]metrics.Sample, 0, len(samples))
	previous := samples[0].Gauges[metrics.GoGCCyclesTotal]
	for _, sample := range samples[1:] {
		current := sample.Gauges[metrics.GoGCCyclesTotal]
		if current > previous {
			out = append(out, sample)
		}
		previous = current
	}
	return out
}

func signedPercentChange(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.MaxFloat64
	}
	return (a - b) / b * 100
}

func counterDelta(samples []metrics.Sample, name string) float64 {
	return float64(counterDeltaUint(samples, name))
}

func counterDeltaUint(samples []metrics.Sample, name string) uint64 {
	if len(samples) < 2 {
		return 0
	}
	first, last := samples[0].Counters[name], samples[len(samples)-1].Counters[name]
	if last < first {
		return 0
	}
	return last - first
}

func percentDelta(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.MaxFloat64
	}
	return math.Abs(a-b) / b * 100
}

func ratio(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.MaxFloat64
	}
	return a / b
}

func observed(value float64, ok bool, suffix string) string {
	if !ok {
		return "missing evidence"
	}
	return fmt.Sprintf("%.3f %s", value, suffix)
}

func humanBytes(v float64) string { return fmt.Sprintf("%.1f MiB", v/(1<<20)) }
func passWord(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}
