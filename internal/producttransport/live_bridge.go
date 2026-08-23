package producttransport

import (
	"context"
	"errors"
	"io"

	"github.com/east-true/dockpilot/internal/livematrix"
	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
)

// LogRelayHandler bridges the bounded, non-persistent log relay to the
// transport-neutral Agent streaming surface.
type LogRelayHandler struct{ Relay *logrelay.Relay }

func (h LogRelayHandler) StreamLogs(ctx context.Context, _ SessionInfo, request LogRequest, sender LogSender) error {
	if h.Relay == nil {
		return ErrHandlerUnavailable
	}
	stream, err := h.Relay.Open(ctx, logrelay.Request{
		ContainerID: request.ContainerID, ProjectUID: request.ProjectUID, Services: append([]string(nil), request.Services...),
		Follow: request.Follow, TailLines: request.TailLines,
		ShowStdout: request.ShowStdout, ShowStderr: request.ShowStderr, Timestamps: request.Timestamps,
		Since: request.Since, Until: request.Until,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sender.Send(LogEvent{
			Data: append([]byte(nil), event.Data...), Stream: string(event.Stream), LineCount: event.LineCount,
			Timestamp: event.Timestamp, DroppedBytes: event.DroppedBytes, DroppedLines: event.DroppedLines,
			Terminal: event.Terminal, Error: event.Error,
		}); err != nil {
			return err
		}
	}
}

// LiveStatsHandler bridges viewer-scoped latest-wins subscriptions to the
// transport-neutral Agent streaming surface.
type LiveStatsHandler struct{ Hub *livestats.Hub }

func (h LiveStatsHandler) StreamStats(ctx context.Context, _ SessionInfo, request StatsRequest, sender StatsSender) error {
	if h.Hub == nil {
		return ErrHandlerUnavailable
	}
	subscription, err := h.Hub.Subscribe(ctx, request.ContainerID)
	if err != nil {
		return err
	}
	defer subscription.Close()
	for {
		sample, err := subscription.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sender.Send(StatsSample{
			ContainerID: sample.ContainerID, ObservedAt: sample.ObservedAt, CPUPercent: sample.CPUPercent,
			MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit, NetworkRX: sample.NetworkRX,
			NetworkTX: sample.NetworkTX, BlockRead: sample.BlockRead, BlockWrite: sample.BlockWrite,
			RestartCount: sample.RestartCount, Health: sample.Health, Uptime: sample.Uptime,
		}); err != nil {
			return err
		}
	}
}

// LiveMatrixHandler bridges one host's whole-frame metrics to the
// transport-neutral Agent streaming surface.
//
// It splits each frame's rows into sampled containers and pending IDs. The
// split is not cosmetic: a member whose first sample has not arrived has no
// numbers to send, and sending a zero-valued sample would report an idle
// container. Naming it as pending says what is true.
type LiveMatrixHandler struct{ Hub *livematrix.Hub }

func (h LiveMatrixHandler) StreamMetricsMatrix(ctx context.Context, _ SessionInfo, _ MetricsMatrixRequest, sender MetricsMatrixSender) error {
	if h.Hub == nil {
		return ErrHandlerUnavailable
	}
	subscription, err := h.Hub.Subscribe(ctx)
	if err != nil {
		return err
	}
	defer subscription.Close()
	for {
		frame, err := subscription.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sender.Send(matrixFrameToWire(frame, subscription.DroppedFrames())); err != nil {
			return err
		}
	}
}

func matrixFrameToWire(frame livematrix.Frame, dropped uint64) MetricsMatrixFrame {
	containers := make([]StatsSample, 0, len(frame.Rows))
	var pending []string
	for _, row := range frame.Rows {
		if row.Pending {
			pending = append(pending, row.ContainerID)
			continue
		}
		sample := row.Sample
		containers = append(containers, StatsSample{
			ContainerID: sample.ContainerID, ObservedAt: sample.ObservedAt, CPUPercent: sample.CPUPercent,
			MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit, NetworkRX: sample.NetworkRX,
			NetworkTX: sample.NetworkTX, BlockRead: sample.BlockRead, BlockWrite: sample.BlockWrite,
			RestartCount: sample.RestartCount, Health: sample.Health, Uptime: sample.Uptime,
		})
	}
	filesystems := make([]ManagedFilesystem, 0, len(frame.Capacity.Filesystems))
	for _, filesystem := range frame.Capacity.Filesystems {
		filesystems = append(filesystems, ManagedFilesystem{
			Path: filesystem.Path, TotalBytes: filesystem.TotalBytes, FreeBytes: filesystem.FreeBytes,
			Unavailable: filesystem.Unavailable, Reason: filesystem.Reason,
		})
	}
	return MetricsMatrixFrame{
		ObservedAt: frame.ObservedAt,
		Workload: WorkloadSummary{
			CPUCapacity: frame.Capacity.CPUCapacity, MemoryCapacity: frame.Capacity.MemoryCapacity,
			ContainersRunning: frame.Running, ContainersTotal: frame.Capacity.ContainersTotal,
			Filesystems: filesystems,
		},
		Containers: containers, PendingContainerIDs: pending, DroppedFrames: dropped,
		MembershipStale: frame.MembershipStale, MembershipReason: frame.MembershipReason,
		WorkloadStale: frame.WorkloadStale, WorkloadReason: frame.WorkloadReason,
	}
}

var _ LogStreamHandler = LogRelayHandler{}
var _ StatsStreamHandler = LiveStatsHandler{}
var _ MetricsMatrixStreamHandler = LiveMatrixHandler{}
