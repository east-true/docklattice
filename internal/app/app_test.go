package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type runtimeFunc struct {
	ready func(context.Context) error
	run   func(context.Context) error
}

func (r runtimeFunc) Ready(ctx context.Context) error { return r.ready(ctx) }
func (r runtimeFunc) Run(ctx context.Context) error   { return r.run(ctx) }

func TestDefaultConfig(t *testing.T) {
	server := DefaultConfig(ModeServer)
	if server.Server.ListenAddress != "127.0.0.1:8080" {
		t.Fatalf("server listen address = %q", server.Server.ListenAddress)
	}
	if server.Server.AgentListenAddress != "127.0.0.1:8443" {
		t.Fatalf("Agent listen address = %q", server.Server.AgentListenAddress)
	}
	if server.Server.AllowPublicBind {
		t.Fatal("public bind is enabled by default")
	}
	if server.Server.StateDir != "/var/lib/dockpilot" {
		t.Fatalf("server state dir = %q", server.Server.StateDir)
	}
	agent := DefaultConfig(ModeAgent)
	if agent.Agent.StateDir != "/var/lib/dockpilot" {
		t.Fatalf("agent state dir = %q", agent.Agent.StateDir)
	}
	if err := server.Defaults.Validate(); err != nil {
		t.Fatalf("V1Defaults validation failed: %v", err)
	}
}

func TestServerPublicBindRequiresOptIn(t *testing.T) {
	cfg := DefaultConfig(ModeServer)
	cfg.Server.ListenAddress = "0.0.0.0"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "--allow-public-bind") {
		t.Fatalf("Validate() error = %v", err)
	}
	cfg.Server.AllowPublicBind = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit public bind failed validation: %v", err)
	}
}

func TestServerAgentPublicBindRequiresOptIn(t *testing.T) {
	cfg := DefaultConfig(ModeServer)
	cfg.Server.AgentListenAddress = "0.0.0.0:8443"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "--allow-public-bind") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentStateDirMustBeAbsolute(t *testing.T) {
	cfg := DefaultConfig(ModeAgent)
	cfg.Agent.StateDir = "state"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestServerStateDirMustBeAbsolute(t *testing.T) {
	cfg := DefaultConfig(ModeServer)
	cfg.Server.StateDir = "state"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestRunRequiresConfiguredRuntime(t *testing.T) {
	err := Run(context.Background(), DefaultConfig(ModeServer), Factories{})
	if !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunChecksReadinessBeforeLifecycle(t *testing.T) {
	want := errors.New("identity state unavailable")
	runCalled := false
	factory := func(Config) (Runtime, error) {
		return runtimeFunc{
			ready: func(context.Context) error { return want },
			run: func(context.Context) error {
				runCalled = true
				return nil
			},
		}, nil
	}
	err := Run(context.Background(), DefaultConfig(ModeServer), Factories{Server: factory})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("Run() error = %v", err)
	}
	if runCalled {
		t.Fatal("runtime ran after readiness failure")
	}
}

func TestRunLifecycleIsCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	factory := func(Config) (Runtime, error) {
		return runtimeFunc{
			ready: func(context.Context) error { return nil },
			run: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DefaultConfig(ModeAgent), Factories{Agent: factory})
	}()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() after cancellation = %v", err)
	}
}

func TestRunRejectsPrematureSuccess(t *testing.T) {
	factory := func(Config) (Runtime, error) {
		return runtimeFunc{
			ready: func(context.Context) error { return nil },
			run:   func(context.Context) error { return nil },
		}, nil
	}
	err := Run(context.Background(), DefaultConfig(ModeAgent), Factories{Agent: factory})
	if !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("Run() error = %v", err)
	}
}
