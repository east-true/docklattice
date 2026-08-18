// Package metrics implements the shared prototype/production metric names from
// ADR A.8. It deliberately has no transport-specific dependencies.
package metrics

import (
	"encoding/json"
	"io"
	"math"
	"runtime"
	runtimemetrics "runtime/metrics"
	"sort"
	"sync"
	"time"
)

const (
	AuditGeneratedTotal                 = "audit_generated_total"
	AuditSyncedTotal                    = "audit_synced_total"
	AuditSyncCursor                     = "audit_sync_cursor"
	AuditSyncLagRecords                 = "audit_sync_lag_records"
	AuditSyncLagSeconds                 = "audit_sync_lag_seconds"
	AuditAckWatermarkStalledSeconds     = "audit_ack_watermark_stalled_seconds"
	AuditAckCursor                      = "audit_ack_cursor"
	OperationProgressEventLatencyMS     = "operation_progress_event_latency_ms"
	CancelAckLatencyMS                  = "cancel_ack_latency_ms"
	QueryEchoRTTMS                      = "query_echo_rtt_ms"
	LogBytesSentTotal                   = "log_bytes_sent_total"
	LogBytesDroppedTotal                = "log_bytes_dropped_total"
	StatsSamplesSentTotal               = "stats_samples_sent_total"
	StatsSamplesDroppedTotal            = "stats_samples_dropped_total"
	OperationOutputTruncatedTotal       = "operation_output_truncated_total"
	AuditCoverageRevisionSeen           = "audit_coverage_revision_seen"
	AuditCoverageRevisionCurrent        = "audit_coverage_revision_current"
	AuditStaleCoverageTotal             = "audit_stale_coverage_total"
	AuditAckRetryTotal                  = "audit_ack_retry_total"
	AuditAckBlockedWhileIngesting       = "audit_ack_blocked_while_ingesting"
	AuditAckBlockedWhileIngestingSecs   = "audit_ack_blocked_while_ingesting_seconds"
	AuditIngestedUnackedRecords         = "audit_ingested_unacked_records"
	AuditIngestedUnackedBytes           = "audit_ingested_unacked_bytes"
	AuditEffectiveGapRecords            = "audit_effective_gap_records"
	AuditAgentGapClaimsTotal            = "audit_agent_gap_claims_total"
	ProcessRSSBytes                     = "process_rss_bytes"
	GoHeapAllocBytes                    = "go_heap_alloc_bytes"
	GoHeapInuseBytes                    = "go_heap_inuse_bytes"
	GoGCCyclesTotal                     = "go_gc_cycles_total"
	GoGCTargetPercent                   = "go_gc_target_percent"
	GoMaxProcs                          = "go_max_procs"
	Goroutines                          = "goroutines"
	OpenFDs                             = "open_fds"
	RlimitNoFileSoft                    = "rlimit_nofile_soft"
	RlimitNoFileHard                    = "rlimit_nofile_hard"
	ProcessCPUAffinityCores             = "process_cpu_affinity_cores"
	BufferBytes                         = "buffer_bytes"
	CgroupMemoryCurrentBytes            = "cgroup_memory_current_bytes"
	CgroupMemoryPeakBytes               = "cgroup_memory_peak_bytes"
	CgroupMemoryMaxBytes                = "cgroup_memory_max_bytes"
	CgroupMemoryHighBytes               = "cgroup_memory_high_bytes"
	CgroupMemorySwapMaxBytes            = "cgroup_memory_swap_max_bytes"
	CgroupCPUQuotaCores                 = "cgroup_cpu_quota_cores"
	CgroupMemoryEventsMaxTotal          = "cgroup_memory_events_max_total"
	CgroupMemoryEventsOOMTotal          = "cgroup_memory_events_oom_total"
	CgroupMemoryEventsOOMKillTotal      = "cgroup_memory_events_oom_kill_total"
	CgroupMemoryAnonBytes               = "cgroup_memory_anon_bytes"
	CgroupMemoryFileBytes               = "cgroup_memory_file_bytes"
	CgroupMemoryInactiveFileBytes       = "cgroup_memory_inactive_file_bytes"
	CgroupMemoryKernelBytes             = "cgroup_memory_kernel_bytes"
	CgroupMemoryKernelStackBytes        = "cgroup_memory_kernel_stack_bytes"
	CgroupMemoryPageTablesBytes         = "cgroup_memory_pagetables_bytes"
	CgroupMemorySlabBytes               = "cgroup_memory_slab_bytes"
	CgroupMemorySlabUnreclaimableBytes  = "cgroup_memory_slab_unreclaimable_bytes"
	CgroupMemorySockBytes               = "cgroup_memory_sock_bytes"
	CgroupMemoryFileDirtyBytes          = "cgroup_memory_file_dirty_bytes"
	CgroupMemoryFileWritebackBytes      = "cgroup_memory_file_writeback_bytes"
	CgroupMemoryWorkingSetApproxBytes   = "cgroup_memory_working_set_approx_bytes"
	CgroupMemoryPressureSomeTotalMicros = "cgroup_memory_pressure_some_total_micros"
	CgroupMemoryPressureFullTotalMicros = "cgroup_memory_pressure_full_total_micros"
)

type histogram struct {
	values []float64
	limit  int
}

// Registry is a bounded in-memory metrics registry. Histograms retain only the
// newest values needed for percentile assertions.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]uint64
	gauges     map[string]float64
	histograms map[string]*histogram
}

func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[string]uint64),
		gauges:     make(map[string]float64),
		histograms: make(map[string]*histogram),
	}
}

func (r *Registry) Add(name string, delta uint64) {
	r.mu.Lock()
	r.counters[name] += delta
	r.mu.Unlock()
}

func (r *Registry) Set(name string, value float64) {
	r.mu.Lock()
	r.gauges[name] = value
	r.mu.Unlock()
}

func (r *Registry) AddGauge(name string, delta float64) {
	r.mu.Lock()
	r.gauges[name] += delta
	if r.gauges[name] < 0 {
		r.gauges[name] = 0
	}
	r.mu.Unlock()
}

func (r *Registry) Observe(name string, value float64) {
	r.mu.Lock()
	h := r.histograms[name]
	if h == nil {
		h = &histogram{limit: 65536}
		r.histograms[name] = h
	}
	if len(h.values) == h.limit {
		copy(h.values, h.values[len(h.values)/2:])
		h.values = h.values[:len(h.values)/2]
	}
	h.values = append(h.values, value)
	r.mu.Unlock()
}

type Distribution struct {
	Count uint64  `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

type Sample struct {
	At             time.Time               `json:"at"`
	ElapsedSeconds float64                 `json:"elapsed_seconds,omitempty"`
	Counters       map[string]uint64       `json:"counters"`
	Gauges         map[string]float64      `json:"gauges"`
	Distributions  map[string]Distribution `json:"distributions,omitempty"`
	Labels         map[string]string       `json:"labels,omitempty"`
}

func (r *Registry) Snapshot(labels map[string]string) Sample {
	r.mu.RLock()
	s := Sample{
		At:            time.Now().UTC(),
		Counters:      cloneMap(r.counters),
		Gauges:        cloneMap(r.gauges),
		Distributions: make(map[string]Distribution, len(r.histograms)),
		Labels:        cloneMap(labels),
	}
	for name, h := range r.histograms {
		values := append([]float64(nil), h.values...)
		s.Distributions[name] = distribution(values)
	}
	r.mu.RUnlock()
	return s
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func distribution(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sort.Float64s(values)
	q := func(p float64) float64 {
		return values[int(math.Ceil(float64(len(values))*p))-1]
	}
	return Distribution{Count: uint64(len(values)), P50: q(.50), P95: q(.95), P99: q(.99), Max: values[len(values)-1]}
}

// CollectRuntime updates process and Go runtime gauges before a snapshot.
func (r *Registry) CollectRuntime() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r.Set(GoHeapAllocBytes, float64(m.HeapAlloc))
	r.Set(GoHeapInuseBytes, float64(m.HeapInuse))
	r.Set(GoGCCyclesTotal, float64(m.NumGC))
	gcTarget := []runtimemetrics.Sample{{Name: "/gc/gogc:percent"}}
	runtimemetrics.Read(gcTarget)
	if gcTarget[0].Value.Kind() == runtimemetrics.KindUint64 {
		r.Set(GoGCTargetPercent, float64(gcTarget[0].Value.Uint64()))
	}
	r.Set(GoMaxProcs, float64(runtime.GOMAXPROCS(0)))
	r.Set(Goroutines, float64(runtime.NumGoroutine()))
	collectLinuxProcess(r)
	collectCgroupV2(r)
}

// WriteJSONL samples once per interval until ctx is done.
func WriteJSONL(ctxDone <-chan struct{}, w io.Writer, interval time.Duration, r *Registry, labels map[string]string) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	enc := json.NewEncoder(w)
	started := time.Now()
	write := func() error {
		r.CollectRuntime()
		sample := r.Snapshot(labels)
		// time.Since retains Go's monotonic clock reading. The wall-clock At field
		// remains useful for correlation but must not be used for rate decisions:
		// WSL/NTP can step CLOCK_REALTIME during a long prototype run.
		sample.ElapsedSeconds = time.Since(started).Seconds()
		return enc.Encode(sample)
	}
	for {
		select {
		case <-ctxDone:
			return write()
		case <-ticker.C:
			if err := write(); err != nil {
				return err
			}
		}
	}
}
