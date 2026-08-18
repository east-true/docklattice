// Package app defines the product process boundary shared by the dockpilot
// command and the concrete Server and Agent runtimes.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/east-true/dockpilot/internal/config"
)

const (
	DefaultServerListenAddress      = "127.0.0.1:8080"
	DefaultServerAgentListenAddress = "127.0.0.1:8443"
	DefaultServerStateDir           = "/var/lib/dockpilot"
	DefaultAgentStateDir            = "/var/lib/dockpilot"
	DefaultAgentServerAddress       = "127.0.0.1:8443"
	DefaultAgentRegistrationURL     = "https://127.0.0.1:8080"
)

var (
	// ErrRuntimeNotConfigured is returned until a mode has a concrete product
	// runtime wired into the command. A product mode must never be a successful
	// no-op.
	ErrRuntimeNotConfigured = errors.New("runtime not configured")
	// ErrRuntimeStopped is returned when a long-running runtime exits cleanly
	// without the process context asking it to stop.
	ErrRuntimeStopped = errors.New("runtime stopped before shutdown was requested")
)

// Mode identifies one of the two supported product process roles.
type Mode string

const (
	ModeServer Mode = "server"
	ModeAgent  Mode = "agent"
)

// ServerConfig contains the process-level Server configuration. The concrete
// Server runtime will extend this boundary as its subsystems are implemented.
type ServerConfig struct {
	ListenAddress      string
	AgentListenAddress string
	AllowPublicBind    bool
	StateDir           string
	TLSCertificateFile string
	TLSPrivateKeyFile  string
}

// PublicBind reports whether ListenAddress can accept non-loopback traffic.
// Hostnames other than localhost are conservatively treated as public.
func (c ServerConfig) PublicBind() (bool, error) {
	for _, address := range []string{c.ListenAddress, c.AgentListenAddress} {
		host, err := listenHost(address)
		if err != nil {
			return false, err
		}
		if strings.EqualFold(host, "localhost") {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return true, nil
		}
	}
	return false, nil
}

func (c ServerConfig) validate() error {
	public, err := c.PublicBind()
	if err != nil {
		return err
	}
	if public && !c.AllowPublicBind {
		return fmt.Errorf("listen address %q is not loopback; pass --allow-public-bind to opt in", c.ListenAddress)
	}
	if strings.TrimSpace(c.StateDir) == "" || !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("server state directory %q must be an absolute path", c.StateDir)
	}
	return nil
}

func listenHost(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("listen address must not be empty")
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		if host == "" {
			return "0.0.0.0", nil
		}
		return strings.Trim(host, "[]"), nil
	}
	if ip := net.ParseIP(strings.Trim(address, "[]")); ip != nil {
		return ip.String(), nil
	}
	if strings.Contains(address, ":") {
		return "", fmt.Errorf("invalid listen address %q", address)
	}
	return address, nil
}

// AgentConfig contains the process-level Agent configuration.
type AgentConfig struct {
	StateDir          string
	ServerAddress     string
	RegistrationURL   string
	ServerCAFile      string
	JoinTokenFile     string
	DisplayName       string
	SelfContainerID   string
	SelfContainerName string
	ProjectRoots      []string
}

func (c AgentConfig) validate() error {
	if strings.TrimSpace(c.StateDir) == "" {
		return errors.New("agent state directory must not be empty")
	}
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("agent state directory %q must be absolute", c.StateDir)
	}
	if strings.TrimSpace(c.ServerAddress) == "" || strings.TrimSpace(c.RegistrationURL) == "" {
		return errors.New("Agent Server address and registration URL are required")
	}
	if c.ServerCAFile == "" || !filepath.IsAbs(c.ServerCAFile) {
		return fmt.Errorf("Agent Server CA file %q must be an absolute path", c.ServerCAFile)
	}
	if c.JoinTokenFile != "" && !filepath.IsAbs(c.JoinTokenFile) {
		return fmt.Errorf("Agent Join Token file %q must be an absolute path", c.JoinTokenFile)
	}
	if strings.TrimSpace(c.DisplayName) == "" {
		return errors.New("Agent display name must not be empty")
	}
	seenRoots := make(map[string]struct{}, len(c.ProjectRoots))
	for _, root := range c.ProjectRoots {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return fmt.Errorf("Agent project root %q must be absolute and clean", root)
		}
		if _, duplicate := seenRoots[root]; duplicate {
			return fmt.Errorf("Agent project root %q is duplicated", root)
		}
		seenRoots[root] = struct{}{}
	}
	return nil
}

// Config is the validated handoff from CLI parsing to a concrete runtime.
type Config struct {
	Mode     Mode
	Defaults config.Defaults
	Server   ServerConfig
	Agent    AgentConfig
}

// DefaultConfig constructs mode-independent v1 defaults and safe process
// defaults. Callers may then apply explicit CLI or file configuration.
func DefaultConfig(mode Mode) Config {
	return Config{
		Mode:     mode,
		Defaults: config.V1Defaults(),
		Server: ServerConfig{
			ListenAddress:      DefaultServerListenAddress,
			AgentListenAddress: DefaultServerAgentListenAddress,
			StateDir:           DefaultServerStateDir,
		},
		Agent: AgentConfig{
			StateDir: DefaultAgentStateDir, ServerAddress: DefaultAgentServerAddress,
			RegistrationURL: DefaultAgentRegistrationURL, ServerCAFile: filepath.Join(DefaultAgentStateDir, "server-ca.crt"),
			DisplayName: "dockpilot-agent",
		},
	}
}

// Validate enforces both the architecture's operational defaults and the
// selected mode's process-level safety boundary.
func (c Config) Validate() error {
	if err := c.Defaults.Validate(); err != nil {
		return fmt.Errorf("v1 defaults: %w", err)
	}
	switch c.Mode {
	case ModeServer:
		if err := c.Server.validate(); err != nil {
			return fmt.Errorf("server configuration: %w", err)
		}
	case ModeAgent:
		if err := c.Agent.validate(); err != nil {
			return fmt.Errorf("agent configuration: %w", err)
		}
	default:
		return fmt.Errorf("unsupported mode %q", c.Mode)
	}
	return nil
}

// Runtime is the explicit readiness and lifecycle boundary for a product
// mode. Ready performs preflight checks; Run owns the long-running lifecycle.
type Runtime interface {
	Ready(context.Context) error
	Run(context.Context) error
}

// Factory creates a runtime from already parsed product configuration.
type Factory func(Config) (Runtime, error)

// Factories are injected so the CLI cannot silently stand in for services
// that have not been implemented yet.
type Factories struct {
	Server Factory
	Agent  Factory
}

func (f Factories) forMode(mode Mode) Factory {
	if mode == ModeServer {
		return f.Server
	}
	if mode == ModeAgent {
		return f.Agent
	}
	return nil
}

// Run validates configuration, checks readiness, and supervises a runtime
// until its context is canceled. A nil factory or premature clean exit is an
// explicit error rather than a successful placeholder service.
func Run(ctx context.Context, cfg Config, factories Factories) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	factory := factories.forMode(cfg.Mode)
	if factory == nil {
		return fmt.Errorf("%s: %w", cfg.Mode, ErrRuntimeNotConfigured)
	}
	runtime, err := factory(cfg)
	if err != nil {
		return fmt.Errorf("configure %s runtime: %w", cfg.Mode, err)
	}
	if runtime == nil {
		return fmt.Errorf("configure %s runtime: %w", cfg.Mode, ErrRuntimeNotConfigured)
	}
	if err := runtime.Ready(ctx); err != nil {
		return fmt.Errorf("%s runtime is not ready: %w", cfg.Mode, err)
	}
	err = runtime.Run(ctx)
	if ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err())) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("%s: %w", cfg.Mode, ErrRuntimeStopped)
	}
	return fmt.Errorf("run %s runtime: %w", cfg.Mode, err)
}
