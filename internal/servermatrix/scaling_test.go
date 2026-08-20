package servermatrix

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/east-true/dockpilot/internal/producttransport"
	pb "github.com/east-true/dockpilot/internal/producttransport/pb"
)

// scalingFrame builds one host's frame at a given container count, shaped like
// a real deployment rather than a flat list: containers belong to projects and
// services, because the grouping and aggregation are part of what is being
// measured.
func scalingFrame(containers int) (producttransport.MetricsMatrixFrame, map[string]ContainerContext) {
	frame := producttransport.MetricsMatrixFrame{
		ObservedAt: time.Unix(1700000000, 0).UTC(),
		Workload: producttransport.WorkloadSummary{
			CPUCapacity: 8, MemoryCapacity: 32 << 30,
			ContainersRunning: uint32(containers), ContainersTotal: uint32(containers),
			Filesystems: []producttransport.ManagedFilesystem{
				{Path: "/srv/projects", TotalBytes: 500 << 30, FreeBytes: 200 << 30},
				{Path: "/var/lib/dockpilot", TotalBytes: 500 << 30, FreeBytes: 200 << 30},
			},
		},
	}
	context := make(map[string]ContainerContext, containers)
	for index := 0; index < containers; index++ {
		id := fmt.Sprintf("%064x", index)
		frame.Containers = append(frame.Containers, producttransport.StatsSample{
			ContainerID: id, ObservedAt: frame.ObservedAt,
			CPUPercent: float64(index%100) + 0.5, MemoryUsage: uint64(index%512) << 20, MemoryLimit: 1 << 30,
			NetworkRX: uint64(index) * 1024, NetworkTX: uint64(index) * 2048,
			BlockRead: uint64(index) * 4096, BlockWrite: uint64(index) * 8192,
			RestartCount: uint64(index % 3), Health: "healthy", Uptime: time.Duration(index) * time.Second,
		})
		// Ten services per project, three containers per service, which is a
		// denser tree than most hosts carry and therefore the more expensive
		// case for grouping.
		context[id] = ContainerContext{
			ProjectUID:  fmt.Sprintf("uid-%02d", index/30),
			ProjectName: fmt.Sprintf("project-%02d", index/30),
			Service:     fmt.Sprintf("service-%02d", (index%30)/3),
			Image:       "registry.example.com/team/service:1.2.3",
		}
	}
	return frame, context
}

func wireBytes(frame producttransport.MetricsMatrixFrame) int {
	wire := &pb.MetricsMatrixFrame{
		ObservedAtUnixNano: frame.ObservedAt.UnixNano(),
		Workload: &pb.WorkloadSummary{
			CpuCapacity: frame.Workload.CPUCapacity, MemoryCapacity: frame.Workload.MemoryCapacity,
			ContainersRunning: frame.Workload.ContainersRunning, ContainersTotal: frame.Workload.ContainersTotal,
		},
	}
	for _, filesystem := range frame.Workload.Filesystems {
		wire.Workload.Filesystems = append(wire.Workload.Filesystems, &pb.ManagedFilesystem{
			Path: filesystem.Path, TotalBytes: filesystem.TotalBytes, FreeBytes: filesystem.FreeBytes,
		})
	}
	for _, sample := range frame.Containers {
		wire.Containers = append(wire.Containers, &pb.StatsSample{
			ContainerId: sample.ContainerID, ObservedAtUnixNano: sample.ObservedAt.UnixNano(),
			CpuPercent: sample.CPUPercent, MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit,
			NetworkRx: sample.NetworkRX, NetworkTx: sample.NetworkTX,
			BlockRead: sample.BlockRead, BlockWrite: sample.BlockWrite,
			RestartCount: sample.RestartCount, Health: sample.Health, UptimeNano: int64(sample.Uptime),
		})
	}
	return proto.Size(wire)
}

// TestMatrixScalingEnvelope measures rather than judges. It records the costs
// that grow with container count and asserts only the two structural claims the
// design makes: stream count follows hosts, and nothing here is quadratic.
//
// Absolute timings depend on the machine and are reported, not asserted. The
// full envelope - Agent RSS, Docker stats cost, file descriptors under a real
// Engine - needs containers this measurement deliberately does not create.
func TestMatrixScalingEnvelope(t *testing.T) {
	for _, containers := range []int{1, 200, 500} {
		t.Run(fmt.Sprintf("%d_containers", containers), func(t *testing.T) {
			frame, contextByID := scalingFrame(containers)

			const rounds = 50
			start := time.Now()
			var view View
			for round := 0; round < rounds; round++ {
				view = assembleForMeasurement(frame, contextByID)
			}
			perFrame := time.Since(start) / rounds
			bytes := wireBytes(frame)

			services := 0
			for _, project := range view.Projects {
				services += len(project.Services)
			}
			t.Logf("containers=%d projects=%d services=%d assemble=%v frame_bytes=%d bytes_per_container=%d",
				containers, len(view.Projects), services, perFrame, bytes, bytes/max(containers, 1))
			t.Logf("containers=%d at a 2s cadence: %.1f KiB/s per watched host",
				containers, float64(bytes)/2/1024)

			// The tree carries every container exactly once, however large it
			// gets. A row lost or duplicated at scale would be invisible in a
			// timing number.
			counted := 0
			for _, project := range view.Projects {
				for _, service := range project.Services {
					counted += len(service.Containers)
				}
			}
			if counted != containers {
				t.Fatalf("the tree carries %d containers, want %d", counted, containers)
			}
			if view.Host.Totals.ContainerCount != uint32(containers) {
				t.Fatalf("the host row counts %d containers, want %d", view.Host.Totals.ContainerCount, containers)
			}
		})
	}
}

// assembleForMeasurement runs the same path a frame takes on its way to a
// viewer: rows, memory-limit judgement, grouping, and every aggregate level.
func assembleForMeasurement(frame producttransport.MetricsMatrixFrame, contextByID map[string]ContainerContext) View {
	hub := &Hub{config: Config{Clock: realClock{}}}
	return hub.assemble(&hostRelay{agentID: "agent-1"}, frame, contextByID, true, "")
}

// Stream count follows hosts, not containers and not viewers. This is the claim
// the protocol change exists to make, and it is the one that must hold at any
// scale; the collector count staying proportional to containers is the cost the
// design states plainly rather than hides.
func TestStreamCountFollowsHostsNotContainersOrViewers(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, _ := newContextHub(t, sessions)
	frame, contextByID := scalingFrame(500)
	source.set(contextByID)

	const viewers = 25
	subscriptions := make([]*Subscription, 0, viewers)
	for index := 0; index < viewers; index++ {
		viewer, err := hub.Subscribe(context.Background(), "agent-1")
		if err != nil {
			t.Fatalf("subscribe viewer %d: %v", index, err)
		}
		subscriptions = append(subscriptions, viewer)
	}
	sessions.current().push(frame)
	for index, viewer := range subscriptions {
		view := nextView(t, viewer)
		if view.Host.Totals.ContainerCount != 500 {
			t.Fatalf("viewer %d saw %d containers", index, view.Host.Totals.ContainerCount)
		}
	}

	if got := sessions.openCount(); got != 1 {
		t.Fatalf("%d viewers of a 500-container host opened %d Agent streams, want 1", viewers, got)
	}
	if got := hub.relayCount(); got != 1 {
		t.Fatalf("a single host holds %d relays", got)
	}
	for _, viewer := range subscriptions {
		_ = viewer.Close()
	}
	waitFor(t, "the relay to be released", func() bool { return hub.relayCount() == 0 })
}
