package acceptance

import (
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/experiment"
	"github.com/east-true/dockpilot/internal/metrics"
)

func TestMissingEvidenceFails(t *testing.T) {
	report := Evaluate(Evidence{Summary: experiment.Summary{Candidate: "candidate", Scenario: 1, OfficialTiming: true}})
	if report.Passed {
		t.Fatal("missing evidence must not pass")
	}
}

func TestCounterDeltaAndSlope(t *testing.T) {
	now := time.Now()
	samples := []metrics.Sample{
		{At: now, Counters: map[string]uint64{"c": 10}, Gauges: map[string]float64{"g": 5}},
		{At: now.Add(time.Second), Counters: map[string]uint64{"c": 15}, Gauges: map[string]float64{"g": 5}},
	}
	if got := counterDeltaUint(samples, "c"); got != 5 {
		t.Fatalf("delta = %d", got)
	}
	if got, ok := maxAgentGaugeSlope([][]metrics.Sample{samples}, "g"); !ok || got != 0 {
		t.Fatalf("slope = %v, %t", got, ok)
	}
}

func TestCounterRateUsesMonotonicElapsedAcrossWallClockStep(t *testing.T) {
	now := time.Now()
	samples := []metrics.Sample{
		{At: now, ElapsedSeconds: 1, Counters: map[string]uint64{"c": 10}},
		{At: now.Add(45 * time.Second), ElapsedSeconds: 2, Counters: map[string]uint64{"c": 30}},
	}
	got, ok := sampleCounterRate(samples, "c")
	if !ok || got != 20 {
		t.Fatalf("rate = %.2f, %t; want 20, true", got, ok)
	}
}

func TestWebsocketRecoveryRequiresZeroPerStreamCreditExtrema(t *testing.T) {
	gauges := map[string]float64{
		"websocket_active_streams":             0,
		"websocket_credit_bytes_available":     0,
		"websocket_credit_messages_available":  0,
		"websocket_stream_credit_bytes_min":    0,
		"websocket_stream_credit_bytes_max":    0,
		"websocket_stream_credit_messages_min": 0,
		"websocket_stream_credit_messages_max": 0,
		"websocket_receive_buffer_bytes":       0,
		"websocket_send_queue_bytes":           0,
		"websocket_send_queue_p0_frames":       0,
		"websocket_send_queue_p1_frames":       0,
		"websocket_send_queue_p2_frames":       0,
		"websocket_send_queue_p3_frames":       0,
		"websocket_send_queue_p4_frames":       0,
	}
	if ok, observed := websocketRecovered(gauges); !ok {
		t.Fatalf("zero state rejected: %s", observed)
	}
	gauges["websocket_stream_credit_bytes_max"] = 1
	if ok, observed := websocketRecovered(gauges); ok {
		t.Fatalf("leaked per-stream credit accepted: %s", observed)
	}
}

func TestExpectedMatrixHasBothCandidatesAndConditions(t *testing.T) {
	groups := expectedMatrixGroups()
	if len(groups) != 26 {
		t.Fatalf("expected groups = %d, want 26", len(groups))
	}
	want := map[string]bool{
		"grpc/netem/scenario-2/rate-100":             false,
		"websocket/loopback/scenario-4/cancellation": false,
	}
	for _, group := range groups {
		if _, ok := want[group]; ok {
			want[group] = true
		}
	}
	for group, found := range want {
		if !found {
			t.Errorf("missing expected group %s", group)
		}
	}
}

func TestCgroupOOMGaugeFailsMemoryCheck(t *testing.T) {
	now := time.Now()
	samples := make([]metrics.Sample, 10)
	for i := range samples {
		samples[i] = metrics.Sample{At: now.Add(time.Duration(i) * time.Second), Gauges: map[string]float64{
			metrics.ProcessRSSBytes:                8 << 20,
			metrics.GoHeapAllocBytes:               2 << 20,
			metrics.GoGCCyclesTotal:                float64(i),
			metrics.CgroupMemoryAnonBytes:          4 << 20,
			metrics.BufferBytes:                    0,
			metrics.CgroupMemoryEventsOOMTotal:     1,
			metrics.CgroupMemoryEventsOOMKillTotal: 0,
		}}
	}
	var checks []Check
	evaluateMemory(Evidence{Server: samples, Agents: [][]metrics.Sample{samples}}, func(check Check) { checks = append(checks, check) }, 512<<20, 256<<20)
	for _, check := range checks {
		if check.ID == "7.server.oom" {
			if check.Passed {
				t.Fatal("an observed cgroup OOM event must fail")
			}
			return
		}
	}
	t.Fatal("server OOM check was not emitted")
}

func TestEchoNetemDegradationUsesMatchedMedianGroups(t *testing.T) {
	report := MatrixReport{Groups: []AggregateGroup{
		{Key: "grpc/loopback/scenario-1/baseline", Candidate: "grpc", Network: "loopback", Scenario: 1, Label: "baseline", Checks: []AggregateCheck{{ID: "diag.echo.p99", Trials: 3, Median: 1.5}}},
		{Key: "grpc/netem/scenario-1/baseline", Candidate: "grpc", Network: "netem", Scenario: 1, Label: "baseline", Checks: []AggregateCheck{{ID: "diag.echo.p99", Trials: 3, Median: 24.0}}},
		{Key: "grpc/loopback/scenario-2/rate-20", Candidate: "grpc", Network: "loopback", Scenario: 2, Label: "rate-20", Checks: []AggregateCheck{{ID: "diag.echo.p99", Trials: 3, Median: 2.0}}},
		{Key: "grpc/netem/scenario-2/rate-20", Candidate: "grpc", Network: "netem", Scenario: 2, Label: "rate-20", Checks: []AggregateCheck{{ID: "diag.echo.p99", Trials: 3, Median: 25.0}}},
	}}
	got, ok := echoNetemDegradation(report, "grpc")
	if !ok || got != 22.75 {
		t.Fatalf("degradation = %.2f, %t; want 22.75, true", got, ok)
	}
}

func TestScaleIncrementUsesMatchedOneAgentBaseline(t *testing.T) {
	evidence := Evidence{
		Summary:        experiment.Summary{ExpectedAgents: 20},
		Baseline:       &experiment.Summary{ExpectedAgents: 1},
		Server:         []metrics.Sample{{Gauges: map[string]float64{metrics.ProcessRSSBytes: 210 << 20}}},
		BaselineServer: []metrics.Sample{{Gauges: map[string]float64{metrics.ProcessRSSBytes: 20 << 20}}},
	}
	var got Check
	evaluateScaleIncrement(evidence, func(check Check) { got = check })
	if !got.Passed || got.Value != 10<<20 {
		t.Fatalf("increment check = %+v; want 10MiB/Agent", got)
	}
}

func TestControlledEvidenceRequiresNetworkProof(t *testing.T) {
	sample := metrics.Sample{Gauges: map[string]float64{
		metrics.CgroupMemoryMaxBytes:     1 << 30,
		metrics.CgroupMemoryHighBytes:    0,
		metrics.CgroupMemorySwapMaxBytes: 0,
		metrics.ProcessCPUAffinityCores:  1,
		metrics.GoMaxProcs:               1,
		metrics.GoGCTargetPercent:        100,
		metrics.RlimitNoFileSoft:         4096,
		metrics.RlimitNoFileHard:         4096,
	}}
	agent := metrics.Sample{Gauges: make(map[string]float64, len(sample.Gauges))}
	for name, value := range sample.Gauges {
		agent.Gauges[name] = value
	}
	agent.Gauges[metrics.CgroupMemoryMaxBytes] = 512 << 20
	evidence := Evidence{Summary: experiment.Summary{OfficialTiming: true, ControlledHarness: true, ExpectedAgents: 1, Network: "netem"}, Server: []metrics.Sample{sample}, Agents: [][]metrics.Sample{{agent}}}
	var check Check
	evaluateControls(evidence, func(got Check) { check = got })
	if check.Passed {
		t.Fatal("missing netem proof must fail controlled evidence")
	}
	evidence.NetworkControl = "qdisc netem 8001: root delay 10ms loss 1%"
	evaluateControls(evidence, func(got Check) { check = got })
	if !check.Passed {
		t.Fatalf("valid netem proof did not pass: %+v", check)
	}
}
