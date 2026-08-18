package dockeradapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
)

type fakeStreamingEngine struct {
	*fakeEngine
	statsBody    io.ReadCloser
	statsOptions client.ContainerStatsOptions
	statsID      string
	logsBody     io.ReadCloser
	logsOptions  client.ContainerLogsOptions
	logsID       string
}

func (e *fakeStreamingEngine) ContainerStats(_ context.Context, id string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	e.statsID, e.statsOptions = id, options
	return client.ContainerStatsResult{Body: e.statsBody}, nil
}

func (e *fakeStreamingEngine) ContainerLogs(_ context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	e.logsID, e.logsOptions = id, options
	return e.logsBody, nil
}

func TestMobyStatsSourceStreamsAndNormalizesSamples(t *testing.T) {
	started := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	responses := []container.StatsResponse{
		{
			Read:     started.Add(time.Second),
			CPUStats: container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: 100}, SystemUsage: 100, OnlineCPUs: 2},
		},
		{
			Read:        started.Add(2 * time.Second),
			CPUStats:    container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: 150}, SystemUsage: 200, OnlineCPUs: 2},
			MemoryStats: container.MemoryStats{Usage: 400, Limit: 1000},
			Networks: map[string]container.NetworkStats{
				"eth0": {RxBytes: 10, TxBytes: 20}, "eth1": {RxBytes: 30, TxBytes: 40},
			},
			BlkioStats: container.BlkioStats{IoServiceBytesRecursive: []container.BlkioStatEntry{
				{Op: "Read", Value: 50}, {Op: "Write", Value: 60},
			}},
		},
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	for _, response := range responses {
		if err := encoder.Encode(response); err != nil {
			t.Fatal(err)
		}
	}
	engine := &fakeStreamingEngine{fakeEngine: &fakeEngine{
		inspect: client.ContainerInspectResult{Container: container.InspectResponse{
			ID: workloadID, RestartCount: 3,
			State: &container.State{StartedAt: started.Format(time.RFC3339Nano), Health: &container.Health{Status: container.Healthy}},
		}},
	}, statsBody: io.NopCloser(bytes.NewReader(encoded.Bytes()))}
	adapter := openFake(t, engine, identified())
	source, err := adapter.LiveStatsSource()
	if err != nil {
		t.Fatal(err)
	}
	var samples []livestats.Sample
	if err := source.Stream(context.Background(), workloadID, func(sample livestats.Sample) error {
		samples = append(samples, sample)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !engine.statsOptions.Stream || engine.statsID != workloadID || len(samples) != 2 {
		t.Fatalf("stats options=%+v id=%q samples=%d", engine.statsOptions, engine.statsID, len(samples))
	}
	if samples[0].CPUPercent != 0 {
		t.Fatalf("first CPU without baseline = %v", samples[0].CPUPercent)
	}
	got := samples[1]
	if got.CPUPercent != 100 || got.MemoryUsage != 400 || got.MemoryLimit != 1000 || got.NetworkRX != 40 ||
		got.NetworkTX != 60 || got.BlockRead != 50 || got.BlockWrite != 60 || got.RestartCount != 3 ||
		got.Health != string(container.Healthy) || got.Uptime != 2*time.Second {
		t.Fatalf("normalized stats = %+v", got)
	}
}

func TestMobyLogSourceDemultiplexesAndForwardsClosedOptions(t *testing.T) {
	var multiplexed bytes.Buffer
	writeDockerFrame(&multiplexed, 1, []byte("out\n"))
	writeDockerFrame(&multiplexed, 2, []byte("err\n"))
	engine := &fakeStreamingEngine{fakeEngine: &fakeEngine{
		inspect: client.ContainerInspectResult{Container: container.InspectResponse{ID: workloadID, Config: &container.Config{Tty: false}}},
	}, logsBody: io.NopCloser(bytes.NewReader(multiplexed.Bytes()))}
	adapter := openFake(t, engine, identified())
	source, err := adapter.LogRelaySource()
	if err != nil {
		t.Fatal(err)
	}
	var chunks []logrelay.Chunk
	request := logrelay.Request{ContainerID: workloadID, Follow: true, TailLines: 25, ShowStdout: true, ShowStderr: true, Timestamps: true}
	if err := source.Stream(context.Background(), request, func(chunk logrelay.Chunk) error {
		chunk.Data = append([]byte(nil), chunk.Data...)
		chunks = append(chunks, chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if engine.logsID != workloadID || !engine.logsOptions.Follow || engine.logsOptions.Tail != "25" || !engine.logsOptions.Timestamps ||
		!engine.logsOptions.ShowStdout || !engine.logsOptions.ShowStderr {
		t.Fatalf("logs options=%+v id=%q", engine.logsOptions, engine.logsID)
	}
	if len(chunks) != 2 || chunks[0].Stream != logrelay.Stdout || string(chunks[0].Data) != "out\n" ||
		chunks[1].Stream != logrelay.Stderr || string(chunks[1].Data) != "err\n" {
		t.Fatalf("demultiplexed chunks = %+v", chunks)
	}
}

func writeDockerFrame(buffer *bytes.Buffer, stream byte, payload []byte) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = buffer.Write(header)
	_, _ = buffer.Write(payload)
}

type blockingReadCloser struct {
	done chan struct{}
	once sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{done: make(chan struct{})}
}
func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.done
	return 0, io.ErrClosedPipe
}
func (r *blockingReadCloser) Close() error { r.once.Do(func() { close(r.done) }); return nil }

type cancelingLogEngine struct {
	*fakeStreamingEngine
	body *blockingReadCloser
}

func (e *cancelingLogEngine) ContainerLogs(ctx context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	e.logsID, e.logsOptions = id, options
	go func() {
		<-ctx.Done()
		_ = e.body.Close()
	}()
	return e.body, nil
}

func TestMobyLogSourceCancellationClosesDockerBody(t *testing.T) {
	body := newBlockingReadCloser()
	base := &fakeStreamingEngine{fakeEngine: &fakeEngine{
		inspect: client.ContainerInspectResult{Container: container.InspectResponse{ID: workloadID, Config: &container.Config{Tty: true}}},
	}}
	engine := &cancelingLogEngine{fakeStreamingEngine: base, body: body}
	adapter := openFake(t, engine, identified())
	source, err := adapter.LogRelaySource()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- source.Stream(ctx, logrelay.Request{ContainerID: workloadID, Follow: true}, func(logrelay.Chunk) error { return nil })
	}()
	cancel()
	select {
	case err := <-result:
		if err == nil || (!errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled)) {
			t.Fatalf("canceled log stream error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Docker log body did not close on cancellation")
	}
}
