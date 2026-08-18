package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type AggregateCheck struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Passed             bool      `json:"passed"`
	Passes             int       `json:"passes"`
	Trials             int       `json:"trials"`
	Median             float64   `json:"median"`
	Values             []float64 `json:"values"`
	FailedObservations []string  `json:"failed_observations,omitempty"`
	OOMAnyFail         bool      `json:"oom_any_fail,omitempty"`
}

type AggregateGroup struct {
	Key       string           `json:"key"`
	Candidate string           `json:"candidate"`
	Network   string           `json:"network"`
	Scenario  int              `json:"scenario"`
	Label     string           `json:"label"`
	Passed    bool             `json:"passed"`
	Checks    []AggregateCheck `json:"checks"`
}

type MatrixReport struct {
	Root              string           `json:"root"`
	Complete          bool             `json:"complete"`
	SingleConnection  map[string]bool  `json:"single_connection_passed"`
	FallbackRequired  bool             `json:"fallback_required"`
	Groups            []AggregateGroup `json:"groups"`
	MissingGroups     []string         `json:"missing_groups,omitempty"`
	Recommendation    string           `json:"recommendation"`
	RecommendationWhy string           `json:"recommendation_why"`
	TieBreak          []TieBreakRow    `json:"tie_break,omitempty"`
}

type TieBreakRow struct {
	Priority   int    `json:"priority"`
	Criterion  string `json:"criterion"`
	CandidateA string `json:"candidate_a"`
	CandidateB string `json:"candidate_b"`
	Favored    string `json:"favored"`
}

func Aggregate(root string) (MatrixReport, error) {
	matrix := MatrixReport{Root: root, SingleConnection: map[string]bool{"grpc": false, "websocket": false}}
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", "scenario-*", "*", "trial-*", "acceptance.json"))
	if err != nil {
		return matrix, err
	}
	type trial struct {
		report Report
		label  string
	}
	groups := make(map[string][]trial)
	for _, path := range paths {
		var report Report
		b, err := os.ReadFile(path)
		if err != nil {
			return matrix, err
		}
		if err := json.Unmarshal(b, &report); err != nil {
			return matrix, fmt.Errorf("%s: %w", path, err)
		}
		label := filepath.Base(filepath.Dir(filepath.Dir(path)))
		key := fmt.Sprintf("%s/%s/scenario-%d/%s", report.Candidate, report.Network, report.Scenario, label)
		groups[key] = append(groups[key], trial{report: report, label: label})
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		trials := groups[key]
		first := trials[0]
		group := AggregateGroup{Key: key, Candidate: first.report.Candidate, Network: first.report.Network, Scenario: first.report.Scenario, Label: first.label, Passed: len(trials) == 3}
		trialNumbers := make(map[int]bool, len(trials))
		byCheck := make(map[string][]Check)
		for _, trial := range trials {
			trialNumbers[trial.report.Trial] = true
			if !trial.report.OfficialTiming {
				group.Passed = false
			}
			for _, check := range trial.report.Checks {
				byCheck[check.ID] = append(byCheck[check.ID], check)
			}
		}
		group.Passed = group.Passed && trialNumbers[1] && trialNumbers[2] && trialNumbers[3] && len(trialNumbers) == 3
		checkIDs := make([]string, 0, len(byCheck))
		for id := range byCheck {
			checkIDs = append(checkIDs, id)
		}
		sort.Strings(checkIDs)
		for _, id := range checkIDs {
			checks := byCheck[id]
			agg := AggregateCheck{ID: id, Name: checks[0].Name, Trials: len(checks)}
			for _, check := range checks {
				if check.Passed {
					agg.Passes++
				}
				agg.Values = append(agg.Values, check.Value)
				if !check.Passed {
					agg.FailedObservations = append(agg.FailedObservations, check.Observed)
				}
				if strings.Contains(strings.ToLower(check.Name), "oom") && !check.Passed {
					agg.OOMAnyFail = true
				}
			}
			sort.Float64s(agg.Values)
			agg.Median = median(agg.Values)
			agg.Passed = len(checks) == 3 && agg.Passes >= 2 && !agg.OOMAnyFail
			group.Passed = group.Passed && agg.Passed
			group.Checks = append(group.Checks, agg)
		}
		matrix.Groups = append(matrix.Groups, group)
	}

	expected := expectedMatrixGroups()
	seen := make(map[string]bool, len(matrix.Groups))
	for _, group := range matrix.Groups {
		seen[group.Key] = true
	}
	for _, key := range expected {
		if !seen[key] {
			matrix.MissingGroups = append(matrix.MissingGroups, key)
		}
	}
	matrix.Complete = len(matrix.MissingGroups) == 0
	for _, candidate := range []string{"grpc", "websocket"} {
		passed := matrix.Complete
		for _, group := range matrix.Groups {
			if group.Candidate == candidate {
				passed = passed && group.Passed
			}
		}
		matrix.SingleConnection[candidate] = passed
	}
	matrix.FallbackRequired = matrix.Complete && !matrix.SingleConnection["grpc"] && !matrix.SingleConnection["websocket"]
	switch {
	case !matrix.Complete:
		matrix.Recommendation = "PENDING"
		matrix.RecommendationWhy = "official matrix is incomplete; missing evidence cannot select a transport"
	case matrix.SingleConnection["grpc"] && !matrix.SingleConnection["websocket"]:
		matrix.Recommendation = "REVERSE_GRPC"
		matrix.RecommendationWhy = "only Candidate A passed every single-connection acceptance group"
	case !matrix.SingleConnection["grpc"] && matrix.SingleConnection["websocket"]:
		matrix.Recommendation = "WEBSOCKET_MULTIPLEXING"
		matrix.RecommendationWhy = "only Candidate B passed every single-connection acceptance group"
	case matrix.SingleConnection["grpc"] && matrix.SingleConnection["websocket"]:
		matrix.Recommendation = "REVERSE_GRPC"
		matrix.RecommendationWhy = "both passed; A.10 prioritizes less hand-written flow-control/cancellation/reconnection correctness logic, which favors gRPC"
	default:
		matrix.Recommendation = "TWO_CONNECTION_FALLBACK_REQUIRED"
		matrix.RecommendationWhy = "both single-connection candidates failed; A.11 requires the two-connection fallback experiment"
	}
	return matrix, nil
}

func AddTieBreakEvidence(report *MatrixReport, repo string) error {
	grpcLines, err := sourceLines(filepath.Join(repo, "internal/candidate/grpcadapter"))
	if err != nil {
		return err
	}
	wsLines, err := sourceLines(filepath.Join(repo, "internal/candidate/wsadapter"))
	if err != nil {
		return err
	}
	grpcDegradation, grpcDegradationOK := echoNetemDegradation(*report, "grpc")
	wsDegradation, wsDegradationOK := echoNetemDegradation(*report, "websocket")
	degradationA := "missing evidence"
	degradationB := "missing evidence"
	degradationFavored := "UNRESOLVED"
	if grpcDegradationOK {
		degradationA = fmt.Sprintf("median Echo p99 +%.3fms", grpcDegradation)
	}
	if wsDegradationOK {
		degradationB = fmt.Sprintf("median Echo p99 +%.3fms", wsDegradation)
	}
	if grpcDegradationOK && wsDegradationOK {
		switch {
		case grpcDegradation < wsDegradation:
			degradationFavored = "A"
		case wsDegradation < grpcDegradation:
			degradationFavored = "B"
		default:
			degradationFavored = "TIE"
		}
	}
	report.TieBreak = []TieBreakRow{
		{1, "adapter implementation size", fmt.Sprintf("%d hand-written non-test Go lines", grpcLines), fmt.Sprintf("%d hand-written non-test Go lines", wsLines), smaller(grpcLines, wsLines)},
		{2, "hand-written correctness logic", "HTTP/2 multiplexing, flow control and cancellation delegated to grpc-go", "framing, five-class scheduler, per-stream byte/message credit and cancellation implemented locally", "A"},
		{3, "dependencies/license", "6 third-party build modules: grpc-go/protobuf + x/net,x/sys,x/text,genproto (Apache-2.0/BSD-3-Clause)", "1 third-party build module: coder/websocket (ISC); shared contract protobuf counted outside adapter", "B"},
		{4, "observability", "standard channelz plus common metrics", "custom scheduler/credit gauges plus common metrics", "A"},
		{5, "version negotiation/skew", "shared handshake; protobuf/gRPC evolution", "shared handshake; custom frame evolution", "A"},
		{6, "20ms RTT + 1% loss degradation", degradationA, degradationB, degradationFavored},
	}
	return nil
}

func echoNetemDegradation(report MatrixReport, candidate string) (float64, bool) {
	groups := make(map[string]AggregateGroup, len(report.Groups))
	for _, group := range report.Groups {
		groups[group.Key] = group
	}
	var deltas []float64
	for _, group := range report.Groups {
		if group.Candidate != candidate || group.Network != "loopback" || (group.Scenario != 1 && group.Scenario != 2) {
			continue
		}
		netemKey := fmt.Sprintf("%s/netem/scenario-%d/%s", candidate, group.Scenario, group.Label)
		netem, ok := groups[netemKey]
		if !ok {
			continue
		}
		loopbackValue, loopbackOK := aggregateCheckValue(group, "diag.echo.p99")
		netemValue, netemOK := aggregateCheckValue(netem, "diag.echo.p99")
		if loopbackOK && netemOK {
			deltas = append(deltas, netemValue-loopbackValue)
		}
	}
	if len(deltas) == 0 {
		return 0, false
	}
	sort.Float64s(deltas)
	return median(deltas), true
}

func aggregateCheckValue(group AggregateGroup, id string) (float64, bool) {
	for _, check := range group.Checks {
		if check.ID == id && check.Trials == 3 {
			return check.Median, true
		}
	}
	return 0, false
}

func sourceLines(dir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return 0, err
	}
	total := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "//") {
				total++
			}
		}
	}
	return total, nil
}

func smaller(a, b int) string {
	if a < b {
		return "A"
	}
	if b < a {
		return "B"
	}
	return "TIE"
}

func expectedMatrixGroups() []string {
	var out []string
	for _, candidate := range []string{"grpc", "websocket"} {
		for _, network := range []string{"loopback", "netem"} {
			out = append(out,
				fmt.Sprintf("%s/%s/scenario-1/baseline", candidate, network),
				fmt.Sprintf("%s/%s/scenario-1/paused", candidate, network),
				fmt.Sprintf("%s/%s/scenario-2/rate-20", candidate, network),
				fmt.Sprintf("%s/%s/scenario-2/rate-50", candidate, network),
				fmt.Sprintf("%s/%s/scenario-2/rate-100", candidate, network),
			)
		}
		out = append(out,
			fmt.Sprintf("%s/loopback/scenario-3/baseline-1-agent", candidate),
			fmt.Sprintf("%s/loopback/scenario-3/scale", candidate),
			fmt.Sprintf("%s/loopback/scenario-4/cancellation", candidate),
		)
	}
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func WriteMatrixJSON(path string, report MatrixReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func WriteMatrixMarkdown(path string, report MatrixReport) error {
	var b strings.Builder
	b.WriteString("# Transport Prototype final report\n\n")
	fmt.Fprintf(&b, "- Matrix complete: **%t**\n", report.Complete)
	fmt.Fprintf(&b, "- Candidate A single connection: **%t**\n", report.SingleConnection["grpc"])
	fmt.Fprintf(&b, "- Candidate B single connection: **%t**\n", report.SingleConnection["websocket"])
	fmt.Fprintf(&b, "- Two-connection fallback required: **%t**\n", report.FallbackRequired)
	fmt.Fprintf(&b, "- Recommendation: **%s** — %s\n", report.Recommendation, report.RecommendationWhy)
	if len(report.MissingGroups) > 0 {
		b.WriteString("\n## Missing groups\n\n")
		for _, key := range report.MissingGroups {
			fmt.Fprintf(&b, "- `%s`\n", key)
		}
	}
	b.WriteString("\n## Median decisions\n\n")
	b.WriteString("| Group | Result |\n|---|---|\n")
	for _, group := range report.Groups {
		fmt.Fprintf(&b, "| `%s` | %s |\n", group.Key, passWord(group.Passed))
	}
	for _, group := range report.Groups {
		fmt.Fprintf(&b, "\n### `%s`\n\n", group.Key)
		b.WriteString("| ID | Check | Result | Passes | Median | Failed observations |\n|---|---|---|---:|---:|---|\n")
		for _, check := range group.Checks {
			failed := strings.Join(check.FailedObservations, "; ")
			fmt.Fprintf(&b, "| %s | %s | %s | %d/%d | %.3f | %s |\n", check.ID, check.Name, passWord(check.Passed), check.Passes, check.Trials, check.Median, failed)
		}
	}
	if len(report.TieBreak) > 0 {
		b.WriteString("\n## A.10 tie-break\n\n| Priority | Criterion | Candidate A | Candidate B | Favored |\n|---:|---|---|---|---|\n")
		for _, row := range report.TieBreak {
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", row.Priority, row.Criterion, row.CandidateA, row.CandidateB, row.Favored)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func WriteDecisionMemo(path string, report MatrixReport) error {
	var b strings.Builder
	b.WriteString("# Transport decision memo\n\n")
	fmt.Fprintf(&b, "Decision: **%s**\n\n", report.Recommendation)
	fmt.Fprintf(&b, "%s.\n\n", report.RecommendationWhy)
	fmt.Fprintf(&b, "Two-connection fallback trigger: **%t**. When false, the fallback was not applied because at least one single-connection candidate passed; when true, this memo is interim until the required fallback experiment completes.\n\n", report.FallbackRequired)
	b.WriteString("The comparison used the frozen thresholds from architecture Appendix A.9. Missing data and failed assertions were not converted into passes; any OOM in the three repetitions fails its group. The two-connection fallback is run only when both single-connection candidates fail, as required by A.11.\n\n")
	if len(report.TieBreak) > 0 {
		b.WriteString("When both candidates pass, A.10 priority 2 overrides priority 1: grpc-go owns substantially more of the correctness-critical flow-control and cancellation machinery, so Candidate A is preferred even if Candidate B has fewer dependency modules or source lines. Netem performance remains an observed criterion in the final report.\n\n")
	}
	b.WriteString("ADR update target after a final selection: §5.1 transport choice and, only if required, §5.3 fallback activation. Prototype-only workload, load-driver, stub queue/store, and the losing adapter remain disposal targets under A.13.\n")
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
