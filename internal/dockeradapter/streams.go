package dockeradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
)

type statsEngine interface {
	ContainerStats(context.Context, string, client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

type logsEngine interface {
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

type StatsSource struct{ engine statsEngine }
type LogSource struct{ engine logsEngine }

func (adapter *Adapter) LiveStatsSource() (*StatsSource, error) {
	engine, ok := adapter.engine.(statsEngine)
	if !ok {
		return nil, errors.New("Docker Engine adapter does not support stats streaming")
	}
	return &StatsSource{engine: engine}, nil
}

func (adapter *Adapter) LogRelaySource() (*LogSource, error) {
	engine, ok := adapter.engine.(logsEngine)
	if !ok {
		return nil, errors.New("Docker Engine adapter does not support log streaming")
	}
	return &LogSource{engine: engine}, nil
}

var _ livestats.Source = (*StatsSource)(nil)
var _ logrelay.Source = (*LogSource)(nil)

func (source *StatsSource) Stream(ctx context.Context, containerID string, emit func(livestats.Sample) error) error {
	if err := validateContainerID(containerID); err != nil {
		return err
	}
	if emit == nil {
		return errors.New("stats emit callback is required")
	}
	inspect, err := source.engine.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("Docker inspect stats target %s: %w", containerID, err)
	}
	metadata := inspectStatsMetadata(inspect.Container)
	result, err := source.engine.ContainerStats(ctx, containerID, client.ContainerStatsOptions{Stream: true})
	if err != nil {
		return fmt.Errorf("Docker stats %s: %w", containerID, err)
	}
	defer result.Body.Close()
	decoder := json.NewDecoder(result.Body)
	var previous container.CPUStats
	for {
		var response container.StatsResponse
		if err := decoder.Decode(&response); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode Docker stats %s: %w", containerID, err)
		}
		sample := statsSample(containerID, response, previous, metadata)
		previous = response.CPUStats
		if err := emit(sample); err != nil {
			return err
		}
	}
}

type statsMetadata struct {
	restartCount uint64
	health       string
	startedAt    time.Time
}

func inspectStatsMetadata(value container.InspectResponse) statsMetadata {
	metadata := statsMetadata{}
	if value.RestartCount > 0 {
		metadata.restartCount = uint64(value.RestartCount)
	}
	if value.State == nil {
		return metadata
	}
	if value.State.Health != nil {
		metadata.health = string(value.State.Health.Status)
	}
	metadata.startedAt, _ = time.Parse(time.RFC3339Nano, value.State.StartedAt)
	return metadata
}

func statsSample(containerID string, response container.StatsResponse, previous container.CPUStats, metadata statsMetadata) livestats.Sample {
	baseline := response.PreCPUStats
	if baseline.CPUUsage.TotalUsage == 0 && baseline.SystemUsage == 0 {
		baseline = previous
	}
	sample := livestats.Sample{
		ContainerID: containerID, ObservedAt: response.Read, CPUPercent: cpuPercent(response.CPUStats, baseline),
		MemoryUsage: response.MemoryStats.Usage, MemoryLimit: response.MemoryStats.Limit,
		RestartCount: metadata.restartCount, Health: metadata.health,
	}
	if !metadata.startedAt.IsZero() && response.Read.After(metadata.startedAt) {
		sample.Uptime = response.Read.Sub(metadata.startedAt)
	}
	for _, network := range response.Networks {
		sample.NetworkRX += network.RxBytes
		sample.NetworkTX += network.TxBytes
	}
	for _, entry := range response.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(entry.Op) {
		case "read":
			sample.BlockRead += entry.Value
		case "write":
			sample.BlockWrite += entry.Value
		}
	}
	return sample
}

func cpuPercent(current, previous container.CPUStats) float64 {
	if previous.CPUUsage.TotalUsage == 0 && previous.SystemUsage == 0 {
		return 0
	}
	if current.CPUUsage.TotalUsage < previous.CPUUsage.TotalUsage || current.SystemUsage <= previous.SystemUsage {
		return 0
	}
	cpuDelta := current.CPUUsage.TotalUsage - previous.CPUUsage.TotalUsage
	systemDelta := current.SystemUsage - previous.SystemUsage
	if cpuDelta == 0 || systemDelta == 0 {
		return 0
	}
	processors := current.OnlineCPUs
	if processors == 0 {
		processors = uint32(len(current.CPUUsage.PercpuUsage))
	}
	if processors == 0 {
		processors = 1
	}
	return float64(cpuDelta) / float64(systemDelta) * float64(processors) * 100
}

func (source *LogSource) Stream(ctx context.Context, request logrelay.Request, emit func(logrelay.Chunk) error) error {
	if err := validateContainerID(request.ContainerID); err != nil {
		return err
	}
	if emit == nil {
		return errors.New("log emit callback is required")
	}
	if !request.ShowStdout && !request.ShowStderr {
		request.ShowStdout, request.ShowStderr = true, true
	}
	inspect, err := source.engine.ContainerInspect(ctx, request.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("Docker inspect log target %s: %w", request.ContainerID, err)
	}
	tail := "all"
	if request.TailLines > 0 {
		tail = strconv.FormatUint(request.TailLines, 10)
	}
	body, err := source.engine.ContainerLogs(ctx, request.ContainerID, client.ContainerLogsOptions{
		ShowStdout: request.ShowStdout, ShowStderr: request.ShowStderr, Follow: request.Follow,
		Tail: tail, Timestamps: request.Timestamps,
	})
	if err != nil {
		return fmt.Errorf("Docker logs %s: %w", request.ContainerID, err)
	}
	defer body.Close()
	stdout := io.Writer(io.Discard)
	stderr := io.Writer(io.Discard)
	if request.ShowStdout {
		stdout = logChunkWriter{ctx: ctx, stream: logrelay.Stdout, emit: emit}
	}
	if request.ShowStderr {
		stderr = logChunkWriter{ctx: ctx, stream: logrelay.Stderr, emit: emit}
	}
	if inspect.Container.Config != nil && inspect.Container.Config.Tty {
		_, err = io.Copy(stdout, body)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, body)
	}
	if err != nil {
		return fmt.Errorf("read Docker logs %s: %w", request.ContainerID, err)
	}
	return nil
}

type logChunkWriter struct {
	ctx    context.Context
	stream logrelay.StreamKind
	emit   func(logrelay.Chunk) error
}

func (writer logChunkWriter) Write(payload []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if err := writer.emit(logrelay.Chunk{
		Data: payload, Stream: writer.stream, LineCount: uint64(bytes.Count(payload, []byte{'\n'})),
	}); err != nil {
		return 0, err
	}
	return len(payload), nil
}
