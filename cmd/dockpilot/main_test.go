package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/east-true/dockpilot/internal/agentid"
	"github.com/east-true/dockpilot/internal/app"
	"github.com/east-true/dockpilot/internal/registration"
	"github.com/east-true/dockpilot/internal/serverbootstrap"
)

type blockingRuntime struct {
	started chan struct{}
}

func (r blockingRuntime) Ready(context.Context) error { return nil }
func (r blockingRuntime) Run(ctx context.Context) error {
	if r.started != nil {
		close(r.started)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestHelp(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--help"}, "dockpilot <server|agent>"},
		{[]string{"server", "--help"}, "--listen"},
		{[]string{"server", "issue-token", "--help"}, "--rejoin-agent-id"},
		{[]string{"agent", "--help"}, "--state-dir"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), tt.args, &stdout, &stderr, app.Factories{}); code != 0 {
			t.Fatalf("run(%v) code = %d, stderr=%q", tt.args, code, stderr.String())
		}
		if output := stdout.String() + stderr.String(); !strings.Contains(output, tt.want) {
			t.Fatalf("run(%v) output = %q, want %q", tt.args, output, tt.want)
		}
	}
}

func TestServerIssueTokenPrintsOneUsablePlaintextCopy(t *testing.T) {
	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{
		"server", "issue-token", "--state-dir", stateDir, "--ttl", "1h",
	}, &stdout, &stderr, app.Factories{}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout does not contain exactly one token line: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), token) || !strings.Contains(stderr.String(), "will not be shown again") {
		t.Fatalf("stderr disclosed token or omitted warning: %q", stderr.String())
	}
	components, err := serverbootstrap.Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer components.Close()
	service, err := registration.New(components.Store, components.Identity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := agentid.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), registration.Request{
		JoinToken: token, AgentID: id, DisplayName: "issued-token-test",
	}); err != nil {
		t.Fatalf("issued token was not usable: %v", err)
	}
}

func TestInvalidMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"worker"}, &stdout, &stderr, app.Factories{}); code != 2 {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown mode") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestUnimplementedModeFailsExplicitly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"server"}, &stdout, &stderr, app.Factories{}); code != 1 {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "runtime not configured") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServerPublicBindContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"server", "--listen", "0.0.0.0"}, &stdout, &stderr, app.Factories{}); code != 2 {
		t.Fatalf("public bind without opt-in code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--allow-public-bind") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stderr.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factories := app.Factories{Server: func(cfg app.Config) (app.Runtime, error) {
		if cfg.Server.ListenAddress != "0.0.0.0" || !cfg.Server.AllowPublicBind {
			t.Fatalf("server config = %#v", cfg.Server)
		}
		return blockingRuntime{}, nil
	}}
	if code := run(ctx, []string{"server", "--listen", "0.0.0.0", "--allow-public-bind"}, &stdout, &stderr, factories); code != 0 {
		t.Fatalf("public bind with opt-in code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAgentDefaultsAndCancelableLifecycle(t *testing.T) {
	started := make(chan struct{})
	var got app.Config
	factories := app.Factories{Agent: func(cfg app.Config) (app.Runtime, error) {
		got = cfg
		return blockingRuntime{started: started}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{"agent"}, &stdout, &stderr, factories)
	}()
	<-started
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if got.Agent.StateDir != "/var/lib/dockpilot" {
		t.Fatalf("state dir = %q", got.Agent.StateDir)
	}
	if got.Agent.ServerAddress != app.DefaultAgentServerAddress || got.Agent.RegistrationURL != app.DefaultAgentRegistrationURL ||
		got.Agent.ServerCAFile != "/var/lib/dockpilot/server-ca.crt" {
		t.Fatalf("Agent connection defaults = %#v", got.Agent)
	}
	if err := got.Defaults.Validate(); err != nil {
		t.Fatalf("CLI did not pass valid V1Defaults: %v", err)
	}
}

func TestAgentStateDirRebasesDefaultCAAndJoinTokenFileIsPrivate(t *testing.T) {
	root := t.TempDir()
	var got app.Config
	factories := app.Factories{Agent: func(cfg app.Config) (app.Runtime, error) {
		got = cfg
		return blockingRuntime{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"agent", "--state-dir", root}, &stdout, &stderr, factories); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if got.Agent.ServerCAFile != filepath.Join(root, "server-ca.crt") {
		t.Fatalf("rebased CA = %q", got.Agent.ServerCAFile)
	}

	secret := filepath.Join(root, "join-token")
	if err := os.WriteFile(secret, []byte("join_token.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := readJoinToken(secret); err != nil || token != "join_token.secret" {
		t.Fatalf("readJoinToken = %q, %v", token, err)
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readJoinToken(secret); err == nil {
		t.Fatal("world-readable Join Token was accepted")
	}
	if err := os.Chmod(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readJoinToken(secret); err == nil {
		t.Fatal("owner-executable Join Token was accepted")
	}
}
