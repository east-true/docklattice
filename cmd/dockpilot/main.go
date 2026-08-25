package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/east-true/dockpilot/internal/app"
	productconfig "github.com/east-true/dockpilot/internal/config"
	"github.com/east-true/dockpilot/internal/registration"
	"github.com/east-true/dockpilot/internal/serverbootstrap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, productFactories()))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, factories app.Factories) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "error: a mode is required")
		printRootUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printRootUsage(stdout)
		return 0
	case "defaults":
		return runDefaults(args[1:], stdout, stderr)
	case string(app.ModeServer):
		return runServer(ctx, args[1:], stdout, stderr, factories)
	case string(app.ModeAgent):
		return runAgent(ctx, args[1:], stdout, stderr, factories)
	default:
		fmt.Fprintf(stderr, "error: unknown mode %q; expected server or agent\n", args[0])
		printRootUsage(stderr)
		return 2
	}
}

func runDefaults(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "error: unexpected defaults argument %q\n", args[0])
		return 2
	}
	defaults := productconfig.V1Defaults()
	if err := defaults.Validate(); err != nil {
		fmt.Fprintf(stderr, "defaults validation failed: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(defaults.Report()); err != nil {
		fmt.Fprintf(stderr, "write defaults report: %v\n", err)
		return 1
	}
	return 0
}

func runServer(ctx context.Context, args []string, stdout, stderr io.Writer, factories app.Factories) int {
	if len(args) != 0 && args[0] == "issue-token" {
		return runIssueToken(ctx, args[1:], stdout, stderr)
	}
	cfg := app.DefaultConfig(app.ModeServer)
	flags := flag.NewFlagSet("dockpilot server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printServerUsage(flags.Output()) }
	flags.StringVar(&cfg.Server.ListenAddress, "listen", app.DefaultServerListenAddress, "listen address; public addresses require --allow-public-bind")
	flags.StringVar(&cfg.Server.AgentListenAddress, "agent-listen", app.DefaultServerAgentListenAddress, "Agent transport listen address; public addresses require --allow-public-bind")
	flags.BoolVar(&cfg.Server.AllowPublicBind, "allow-public-bind", false, "explicitly allow a non-loopback listen address")
	flags.StringVar(&cfg.Server.StateDir, "state-dir", app.DefaultServerStateDir, "absolute path for separate Server database and identity state")
	flags.StringVar(&cfg.Server.TLSCertificateFile, "tls-cert", "", "TLS certificate PEM (default <state-dir>/tls/server.crt)")
	flags.StringVar(&cfg.Server.TLSPrivateKeyFile, "tls-key", "", "TLS private key PEM (default <state-dir>/tls/server.key)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected server argument %q\n", flags.Arg(0))
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	public, _ := cfg.Server.PublicBind()
	if public {
		fmt.Fprintf(stderr, "WARNING: server public bind explicitly enabled for %q; protect access at the deployment boundary\n", cfg.Server.ListenAddress)
	}
	return runConfigured(ctx, cfg, stdout, stderr, factories)
}

func runIssueToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dockpilot server issue-token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printIssueTokenUsage(flags.Output()) }
	stateDir := app.DefaultServerStateDir
	lifetime := 15 * time.Minute
	rejoinAgentID := ""
	flags.StringVar(&stateDir, "state-dir", stateDir, "absolute path to Server state")
	flags.DurationVar(&lifetime, "ttl", lifetime, "one-time token lifetime")
	flags.StringVar(&rejoinAgentID, "rejoin-agent-id", "", "bind token to one existing Agent identity")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || !filepath.IsAbs(stateDir) || lifetime <= 0 {
		fmt.Fprintln(stderr, "configuration error: absolute --state-dir, positive --ttl, and no positional arguments are required")
		return 2
	}
	components, err := serverbootstrap.Open(ctx, stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "issue Join Token: %v\n", err)
		return 1
	}
	defer components.Close()
	service, err := registration.New(components.Store, components.Identity)
	if err != nil {
		fmt.Fprintf(stderr, "issue Join Token: %v\n", err)
		return 1
	}
	var issued registration.IssuedToken
	if rejoinAgentID == "" {
		issued, err = service.IssueJoinToken(ctx, lifetime)
	} else {
		issued, err = service.IssueRejoinToken(ctx, rejoinAgentID, lifetime)
	}
	if err != nil {
		fmt.Fprintf(stderr, "issue Join Token: %v\n", err)
		return 1
	}
	// The token is deliberately the only stdout field so operators can redirect
	// it straight into an owner-only secret file without parsing logs.
	fmt.Fprintln(stdout, issued.Token)
	fmt.Fprintf(stderr, "Join Token %s expires at %s; plaintext will not be shown again\n", issued.ID, issued.ExpiresAt.UTC().Format(time.RFC3339))
	return 0
}

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer, factories app.Factories) int {
	cfg := app.DefaultConfig(app.ModeAgent)
	flags := flag.NewFlagSet("dockpilot agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printAgentUsage(flags.Output()) }
	flags.StringVar(&cfg.Agent.StateDir, "state-dir", app.DefaultAgentStateDir, "absolute path for durable Agent state")
	flags.StringVar(&cfg.Agent.ServerAddress, "server", app.DefaultAgentServerAddress, "Server Agent transport address")
	flags.StringVar(&cfg.Agent.RegistrationURL, "registration-url", app.DefaultAgentRegistrationURL, "HTTPS Server registration base URL")
	cfg.Agent.ServerCAFile = ""
	flags.StringVar(&cfg.Agent.ServerCAFile, "server-ca", "", "PEM CA/certificate used to authenticate the Server (default <state-dir>/server-ca.crt)")
	flags.StringVar(&cfg.Agent.JoinTokenFile, "join-token-file", "", "0600 file containing the one-time Join Token")
	flags.StringVar(&cfg.Agent.DisplayName, "display-name", cfg.Agent.DisplayName, "Agent display name used during registration")
	flags.StringVar(&cfg.Agent.SelfContainerID, "self-container-id", "", "explicit Agent container ID fallback")
	flags.StringVar(&cfg.Agent.SelfContainerName, "self-container-name", "", "explicit Agent container name fallback")
	flags.Var((*stringListFlag)(&cfg.Agent.ProjectRoots), "project-root", "absolute identical-path discovery bind mount (repeatable)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected agent argument %q\n", flags.Arg(0))
		return 2
	}
	if cfg.Agent.ServerCAFile == "" {
		cfg.Agent.ServerCAFile = filepath.Join(cfg.Agent.StateDir, "server-ca.crt")
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	return runConfigured(ctx, cfg, stdout, stderr, factories)
}

func runConfigured(ctx context.Context, cfg app.Config, _, stderr io.Writer, factories app.Factories) int {
	if err := app.Run(ctx, cfg, factories); err != nil {
		fmt.Fprintf(stderr, "dockpilot %s failed: %v\n", cfg.Mode, err)
		return 1
	}
	return 0
}

func printRootUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: dockpilot <server|agent|defaults> [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Modes:")
	fmt.Fprintln(w, "  server  run the Dockpilot control plane")
	fmt.Fprintln(w, "  agent   run the Docker host controller")
	fmt.Fprintln(w, "  defaults  print the machine-readable v1 operational defaults")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Use 'dockpilot <mode> --help' for mode options.")
}

func printServerUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: dockpilot server [options]")
	fmt.Fprintln(w, "       dockpilot server issue-token [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintf(w, "  --listen address         listen address (default %s)\n", app.DefaultServerListenAddress)
	fmt.Fprintf(w, "  --agent-listen address   Agent transport address (default %s)\n", app.DefaultServerAgentListenAddress)
	fmt.Fprintln(w, "  --allow-public-bind      explicitly allow a non-loopback listen address")
	fmt.Fprintf(w, "  --state-dir path         durable Server state directory (default %s)\n", app.DefaultServerStateDir)
	fmt.Fprintln(w, "  --tls-cert path          TLS certificate PEM (default <state-dir>/tls/server.crt)")
	fmt.Fprintln(w, "  --tls-key path           TLS private key PEM (default <state-dir>/tls/server.key)")
}

func printIssueTokenUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: dockpilot server issue-token [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintf(w, "  --state-dir path         durable Server state directory (default %s)\n", app.DefaultServerStateDir)
	fmt.Fprintln(w, "  --ttl duration           one-time token lifetime (default 15m)")
	fmt.Fprintln(w, "  --rejoin-agent-id id     bind token to an existing Agent identity")
}

func printAgentUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: dockpilot agent [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintf(w, "  --state-dir path         durable Agent state directory (default %s)\n", app.DefaultAgentStateDir)
	fmt.Fprintf(w, "  --server address         Server Agent transport address (default %s)\n", app.DefaultAgentServerAddress)
	fmt.Fprintf(w, "  --registration-url URL   HTTPS registration base URL (default %s)\n", app.DefaultAgentRegistrationURL)
	fmt.Fprintln(w, "  --server-ca path         PEM CA/certificate used to authenticate the Server")
	fmt.Fprintln(w, "  --join-token-file path   0600 file containing the one-time Join Token")
	fmt.Fprintln(w, "  --display-name name      Agent display name used during registration")
	fmt.Fprintln(w, "  --self-container-id id   explicit Agent container ID fallback")
	fmt.Fprintln(w, "  --self-container-name n  explicit Agent container name fallback")
	fmt.Fprintln(w, "  --project-root path      identical-path discovery bind mount (repeatable)")
}

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}
