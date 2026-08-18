package workload

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/east-true/dockpilot/internal/contract"
	pb "github.com/east-true/dockpilot/internal/contract/pb"
	"github.com/east-true/dockpilot/internal/metrics"
	"github.com/east-true/dockpilot/internal/transport"
)

func TestPadProtoHitsControlledSizes(t *testing.T) {
	record := &pb.AuditRecord{AgentId: "agent-001", Incarnation: 1, Seq: 100, OccurredAtUnixNano: 1, Kind: "managed-like"}
	padProto(record, 512, func(n int) { record.Payload = make([]byte, n) })
	if got := proto.Size(record); got != 512 {
		t.Fatalf("audit record size = %d, want 512", got)
	}
	sample := &pb.StatsSample{Target: "container-1", SampledAtUnixNano: 1, CpuPercent: 50, MemoryBytes: 1024, MemoryLimitBytes: 512 << 20}
	padProto(sample, 1024, func(n int) { sample.Padding = make([]byte, n) })
	if got := proto.Size(sample); got != 1024 {
		t.Fatalf("stats sample size = %d, want 1024", got)
	}
}

func TestSyntheticAuditGeneratorProfiles(t *testing.T) {
	for _, rate := range []int{10, 20, 50, 100} {
		for _, profile := range []struct {
			name string
			size int
		}{
			{name: "small", size: 256},
			{name: "medium", size: 512},
		} {
			for _, mode := range []string{"managed-like", "observed-like"} {
				t.Run(fmt.Sprintf("%d/%s/%s", rate, profile.name, mode), func(t *testing.T) {
					reg := metrics.NewRegistry()
					cfg := DefaultConfig()
					cfg.AuditRecordsPerSecond = rate
					cfg.AuditPayloadBytes = profile.size
					cfg.AuditMode = mode
					svc := NewService(cfg, reg)
					defer svc.Close()

					select {
					case record := <-svc.auditQ:
						if got := proto.Size(record); got != profile.size {
							t.Fatalf("serialized size = %d, want %d", got, profile.size)
						}
						if record.Kind != mode {
							t.Fatalf("mode = %q, want %q", record.Kind, mode)
						}
					case <-time.After(150 * time.Millisecond):
						t.Fatalf("rate %d/s produced no record", rate)
					}
				})
			}
		}
	}
}

func TestRegisterReturnsBoundTransportSession(t *testing.T) {
	reg := metrics.NewRegistry()
	cfg := DefaultConfig()
	cfg.AuditRecordsPerSecond = 0
	svc := NewService(cfg, reg)
	defer svc.Close()
	svc.BindSession("0123456789abcdef0123456789abcdef")
	response, err := svc.Register(context.Background(), &pb.RegisterRequest{AgentId: cfg.AgentID, ProtocolVersion: transport.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	if response.SessionId != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("session id = %q", response.SessionId)
	}
}

func TestOperationOutputAlwaysDrainsIntoBoundedTail(t *testing.T) {
	reg := metrics.NewRegistry()
	cfg := DefaultConfig()
	cfg.AuditRecordsPerSecond = 0
	cfg.OperationDuration = 100 * time.Millisecond
	cfg.OperationLinesPerSecond = 20_000
	cfg.OperationLineBytes = 2_000
	svc := NewService(cfg, reg)
	defer svc.Close()

	blocked := &blockingSender{started: make(chan struct{})}
	outputCtx, cancelOutput := context.WithCancel(context.Background())
	outputDone := make(chan error, 1)
	go func() {
		outputDone <- svc.OperationOutput(outputCtx, &pb.OperationOutputRequest{OperationId: "operation"}, contract.NewSink[*pb.OperationOutputChunk](blocked))
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("output sender did not block")
	}

	progress := &collectingSender{}
	if err := svc.OperationProgress(context.Background(), &pb.OperationProgressRequest{}, contract.NewSink[*pb.OperationProgressEvent](progress)); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if progress.count() != 4 {
		t.Fatalf("progress events = %d, want 4", progress.count())
	}
	snapshot := reg.Snapshot(nil)
	if got := snapshot.Gauges[metrics.BufferBytes]; got > 66<<10 {
		t.Fatalf("bounded tail = %.0f bytes", got)
	}
	if snapshot.Counters[metrics.OperationOutputTruncatedTotal] == 0 {
		t.Fatal("blocked output did not report truncation")
	}
	cancelOutput()
	select {
	case <-outputDone:
	case <-time.After(time.Second):
		t.Fatal("output handler did not stop")
	}
	if got := reg.Snapshot(nil).Gauges[metrics.BufferBytes]; got != 0 {
		t.Fatalf("buffer after cancellation = %.0f", got)
	}
}

func TestLogGeneratorSustainsConfiguredByteRate(t *testing.T) {
	reg := metrics.NewRegistry()
	cfg := DefaultConfig()
	cfg.AuditRecordsPerSecond = 0
	svc := NewService(cfg, reg)
	defer svc.Close()
	sender := &collectingSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := svc.StreamLogs(ctx, &pb.StreamLogsRequest{StreamId: "rate", ByteRate: 200 << 10, LineSize: 200}, contract.NewSink[*pb.LogChunk](sender))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StreamLogs ended with %v", err)
	}
	bytes := sender.count() * 200
	if bytes < int(float64(200<<10)*.45) {
		t.Fatalf("generated %d bytes in 500ms; want at least 90%% of configured rate", bytes)
	}
}

type blockingSender struct {
	once    sync.Once
	started chan struct{}
}

func (s *blockingSender) ID() transport.ExchangeID { return 1 }
func (s *blockingSender) Send(ctx context.Context, _ []byte) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

type collectingSender struct {
	mu       sync.Mutex
	messages [][]byte
}

func (s *collectingSender) ID() transport.ExchangeID { return 2 }
func (s *collectingSender) Send(_ context.Context, msg []byte) error {
	s.mu.Lock()
	s.messages = append(s.messages, append([]byte(nil), msg...))
	s.mu.Unlock()
	return nil
}
func (s *collectingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}
