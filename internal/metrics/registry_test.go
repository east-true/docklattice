package metrics

import "testing"

func TestRegistrySnapshotAndPercentiles(t *testing.T) {
	r := NewRegistry()
	r.Add(AuditGeneratedTotal, 2)
	r.Set(AuditSyncCursor, 7)
	for i := 1; i <= 100; i++ {
		r.Observe(QueryEchoRTTMS, float64(i))
	}
	s := r.Snapshot(map[string]string{"candidate": "test"})
	if s.Counters[AuditGeneratedTotal] != 2 || s.Gauges[AuditSyncCursor] != 7 {
		t.Fatalf("snapshot = %+v", s)
	}
	d := s.Distributions[QueryEchoRTTMS]
	if d.Count != 100 || d.P50 != 50 || d.P95 != 95 || d.P99 != 99 || d.Max != 100 {
		t.Fatalf("distribution = %+v", d)
	}
	r.CollectRuntime()
	if r.Snapshot(nil).Gauges[Goroutines] < 1 {
		t.Fatal("runtime collector did not record goroutines")
	}
}
