package serverapi

import (
	"context"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/webui"
)

// TestDashboardAnswersWhileOneAgentIsUnreachable is the multi-Agent isolation
// contract: one host being unreachable is that host's problem, and the fleet
// view must still answer.
//
// The failure it pins was found by the multi-agent lab. Cutting a single Agent
// off the network left `GET /api/v1/dashboard` returning nothing at all for
// more than twenty seconds, for every Agent, while the other two were healthy.
// A partitioned Agent does not reset its connection - its packets are dropped -
// so the heartbeat the dashboard performs per host hangs until something above
// it gives up. The dashboard waits for every host probe before it answers, so
// the slowest host sets the latency of the whole fleet view.
//
// The Server's own liveness loop already bounds a heartbeat at
// producttransport.DefaultHeartbeatTimeout. The dashboard's did not.
func TestDashboardAnswersWhileOneAgentIsUnreachable(t *testing.T) {
	t.Parallel()

	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-healthy", "Healthy", `{"fs_read": true, "fs_write": true}`)
	insertAgent(t, ctx, store, "agent-unreachable", "Unreachable", `{"fs_read": true, "fs_write": true}`)

	healthy := newFakeSession("agent-healthy")
	healthy.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true}
	if err := registry.Register(healthy); err != nil {
		t.Fatal(err)
	}
	unreachable := newFakeSession("agent-unreachable")
	unreachable.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true}
	unreachable.heartbeatBlocks = true
	if err := registry.Register(unreachable); err != nil {
		t.Fatal(err)
	}

	// No deadline on the caller: a browser request carries none either, and
	// relying on one would only move the hang rather than remove it.
	type result struct {
		hosts []string
		took  time.Duration
		err   error
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		dashboard, err := backend.Dashboard(context.Background())
		names := make([]string, 0, len(dashboard.Hosts))
		for _, host := range dashboard.Hosts {
			names = append(names, host.ID)
		}
		done <- result{hosts: names, took: time.Since(started), err: err}
	}()

	// The bound is generous on purpose. What is being asserted is that the
	// answer is bounded at all, not that it is fast.
	var got result
	select {
	case got = <-done:
	case <-time.After(producttransport.DefaultHeartbeatTimeout + 20*time.Second):
		t.Fatal("the dashboard never answered while one Agent was unreachable")
	}
	if got.err != nil {
		t.Fatalf("dashboard error = %v, want an answer", got.err)
	}
	if len(got.hosts) != 2 {
		t.Fatalf("dashboard hosts = %v, want both agents listed", got.hosts)
	}
	if got.took > producttransport.DefaultHeartbeatTimeout+10*time.Second {
		t.Fatalf("dashboard took %s; an unreachable Agent must not set the latency of the fleet view", got.took)
	}
}

// TestDashboardReportsAnUnreachableAgentAsUnavailable checks the other half:
// having answered, the answer has to be honest. A host whose heartbeat did not
// come back is not a host with working capabilities.
func TestDashboardReportsAnUnreachableAgentAsUnavailable(t *testing.T) {
	t.Parallel()

	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-healthy", "Healthy", `{"fs_read": true, "fs_write": true}`)
	insertAgent(t, ctx, store, "agent-unreachable", "Unreachable", `{"fs_read": true, "fs_write": true}`)

	healthy := newFakeSession("agent-healthy")
	healthy.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true}
	if err := registry.Register(healthy); err != nil {
		t.Fatal(err)
	}
	unreachable := newFakeSession("agent-unreachable")
	unreachable.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true}
	unreachable.heartbeatBlocks = true
	if err := registry.Register(unreachable); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		dashboard webui.Dashboard
		err       error
	}
	answered := make(chan answer, 1)
	go func() {
		value, err := backend.Dashboard(context.Background())
		answered <- answer{dashboard: value, err: err}
	}()
	var got answer
	select {
	case got = <-answered:
	case <-time.After(producttransport.DefaultHeartbeatTimeout + 20*time.Second):
		t.Fatal("the dashboard never answered while one Agent was unreachable")
	}
	if got.err != nil {
		t.Fatalf("dashboard error = %v", got.err)
	}
	dashboard := got.dashboard

	var healthyHost, blockedHost *struct {
		id           string
		state        string
		docker       bool
		dockerReason string
	}
	for index := range dashboard.Hosts {
		host := dashboard.Hosts[index]
		view := &struct {
			id           string
			state        string
			docker       bool
			dockerReason string
		}{
			id: host.ID, state: host.State,
			docker: host.Capabilities.Docker.Enabled, dockerReason: host.Capabilities.Docker.Reason,
		}
		switch host.ID {
		case "agent-healthy":
			healthyHost = view
		case "agent-unreachable":
			blockedHost = view
		}
	}
	if healthyHost == nil || blockedHost == nil {
		t.Fatalf("hosts = %+v, want both agents", dashboard.Hosts)
	}
	if healthyHost.state != "ACTIVE" || !healthyHost.docker {
		t.Fatalf("the healthy Agent was reported as %+v", *healthyHost)
	}
	if blockedHost.docker {
		t.Fatalf("an unreachable Agent was reported with Docker enabled: %+v", *blockedHost)
	}
	if blockedHost.dockerReason == "" {
		t.Fatal("an unreachable Agent was reported with no reason for its disabled capability")
	}
}
