package producttransport

import (
	"context"
	"errors"
	"io"

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

var _ LogStreamHandler = LogRelayHandler{}
var _ StatsStreamHandler = LiveStatsHandler{}
