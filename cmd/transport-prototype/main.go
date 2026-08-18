// transport-prototype is the disposable Appendix A experiment binary. It has
// no Docker/Compose/product behavior and must not be installed as Dockpilot.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/east-true/dockpilot/internal/acceptance"
	"github.com/east-true/dockpilot/internal/candidate/grpcadapter"
	"github.com/east-true/dockpilot/internal/candidate/wsadapter"
	"github.com/east-true/dockpilot/internal/contract"
	"github.com/east-true/dockpilot/internal/experiment"
	"github.com/east-true/dockpilot/internal/metrics"
	"github.com/east-true/dockpilot/internal/transport"
	"github.com/east-true/dockpilot/internal/workload"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: transport-prototype <local|serve|agent|cert|report|aggregate> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "local":
		err = runLocal(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "cert":
		err = runCert(os.Args[2:])
	case "report":
		err = runReport(os.Args[2:])
	case "aggregate":
		err = runAggregate(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func runAggregate(args []string) error {
	fs := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	root := fs.String("root", "artifacts/transport-prototype/official", "official matrix root")
	repo := fs.String("repo", ".", "repository root for tie-break source evidence")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := acceptance.Aggregate(*root)
	if err != nil {
		return err
	}
	if err := acceptance.AddTieBreakEvidence(&report, *repo); err != nil {
		return err
	}
	if err := acceptance.WriteMatrixJSON(filepath.Join(*root, "final-report.json"), report); err != nil {
		return err
	}
	if err := acceptance.WriteMatrixMarkdown(filepath.Join(*root, "final-report.md"), report); err != nil {
		return err
	}
	if err := acceptance.WriteDecisionMemo(filepath.Join(*root, "decision-memo.md"), report); err != nil {
		return err
	}
	fmt.Printf("%s\n", report.Recommendation)
	if !report.Complete {
		return errors.New("official matrix is incomplete")
	}
	if report.FallbackRequired {
		return errors.New("both single-connection candidates failed; A.11 two-connection fallback is required")
	}
	return nil
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	runDir := fs.String("run", "", "trial output directory")
	baselineDir := fs.String("baseline", "", "matching scenario-1 baseline directory")
	requireOfficial := fs.Bool("require-official", true, "fail scaled/non-controlled evidence")
	output := fs.String("output", "", "report output directory; defaults to run directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	evidence, err := acceptance.Load(*runDir)
	if err != nil {
		return err
	}
	evidence.RequireExact = *requireOfficial
	if *baselineDir != "" {
		baseline, err := acceptance.Load(*baselineDir)
		if err != nil {
			return err
		}
		evidence.Baseline = &baseline.Summary
		evidence.BaselineServer = baseline.Server
	}
	report := acceptance.Evaluate(evidence)
	dir := *output
	if dir == "" {
		dir = *runDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := acceptance.WriteJSON(filepath.Join(dir, "acceptance.json"), report); err != nil {
		return err
	}
	if err := acceptance.WriteMarkdown(filepath.Join(dir, "acceptance.md"), []acceptance.Report{report}); err != nil {
		return err
	}
	fmt.Printf("%s\n", map[bool]string{true: "PASS", false: "FAIL"}[report.Passed])
	if !report.Passed {
		return errors.New("acceptance criteria not met")
	}
	return nil
}

type commonFlags struct {
	candidate string
	config    string
	output    string
	network   string
	trial     int
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.candidate, "candidate", "grpc", "grpc or websocket")
	fs.StringVar(&c.config, "config", "", "experiment config JSON")
	fs.StringVar(&c.output, "output", "artifacts/transport-prototype", "output directory")
	fs.StringVar(&c.network, "network", "loopback", "network condition label")
	fs.IntVar(&c.trial, "trial", 1, "trial number")
	return c
}

func runLocal(args []string) error {
	fs := flag.NewFlagSet("local", flag.ContinueOnError)
	common := addCommon(fs)
	scenario := fs.Int("scenario", 1, "scenario 1-4")
	scale := fs.Float64("time-scale", 1, "duration scale; 1 is the official protocol")
	auditRate := fs.Int("audit-rate", 0, "override audit records/sec")
	pause := fs.Bool("pause-log", true, "pause log #1 consumer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := experiment.DefaultConfig(*scenario)
	cfg.TimeScale = *scale
	cfg.PauseLog = *pause
	if *auditRate > 0 {
		cfg.AuditRate = *auditRate
	}
	if err := os.MkdirAll(common.output, 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(common.output, "config.json")
	if err := writeJSON(configPath, cfg); err != nil {
		return err
	}
	certPath, keyPath, err := generateCertificate(common.output, []string{"127.0.0.1", "localhost"})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	serverTLS, err := loadServerTLS(certPath, keyPath)
	if err != nil {
		return err
	}
	acceptor, err := newAcceptor(common.candidate, listener, serverTLS, transport.DefaultLimits())
	if err != nil {
		return err
	}
	defer acceptor.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	agents := make([]*exec.Cmd, 0, cfg.Agents)
	for i := 0; i < cfg.Agents; i++ {
		agentRaw := filepath.Join(common.output, fmt.Sprintf("agent-%03d.jsonl", i+1))
		cmd := exec.CommandContext(ctx, os.Args[0], "agent",
			"--candidate", common.candidate,
			"--endpoint", listener.Addr().String(),
			"--ca", certPath,
			"--config", configPath,
			"--raw", agentRaw,
			"--agent-id", fmt.Sprintf("agent-%03d", i+1),
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		agents = append(agents, cmd)
	}
	callers, err := acceptAgents(ctx, acceptor, cfg.Agents)
	if err != nil {
		return err
	}
	defer closeCallers(callers)
	reg := metrics.NewRegistry()
	reg.Set(metrics.BufferBytes, 0)
	summary, runErr := experiment.Run(ctx, callers, cfg, reg, experiment.RunOptions{Candidate: common.candidate, Network: common.network, Trial: common.trial, RawPath: filepath.Join(common.output, "server.jsonl")})
	closeCallers(callers)
	for _, cmd := range agents {
		if err := cmd.Wait(); err != nil && runErr == nil {
			runErr = fmt.Errorf("agent process: %w", err)
		}
	}
	if err := experiment.WriteSummary(filepath.Join(common.output, "summary.json"), summary); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	common := addCommon(fs)
	listenAddr := fs.String("listen", "127.0.0.1:8443", "TLS listen address")
	certPath := fs.String("cert", "", "server certificate PEM")
	keyPath := fs.String("key", "", "server private key PEM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := readConfig(common.config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(common.output, 0o700); err != nil {
		return err
	}
	serverTLS, err := loadServerTLS(*certPath, *keyPath)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return err
	}
	acceptor, err := newAcceptor(common.candidate, listener, serverTLS, transport.DefaultLimits())
	if err != nil {
		return err
	}
	defer acceptor.Close()
	fmt.Printf("READY %s\n", listener.Addr())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	callers, err := acceptAgents(ctx, acceptor, cfg.Agents)
	if err != nil {
		return err
	}
	defer closeCallers(callers)
	summary, err := experiment.Run(ctx, callers, cfg, metrics.NewRegistry(), experiment.RunOptions{Candidate: common.candidate, Network: common.network, Trial: common.trial, RawPath: filepath.Join(common.output, "server.jsonl")})
	if writeErr := experiment.WriteSummary(filepath.Join(common.output, "summary.json"), summary); writeErr != nil && err == nil {
		err = writeErr
	}
	return err
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	candidate := fs.String("candidate", "grpc", "grpc or websocket")
	endpoint := fs.String("endpoint", "127.0.0.1:8443", "server host:port")
	caPath := fs.String("ca", "", "server CA/certificate PEM")
	configPath := fs.String("config", "", "experiment config JSON")
	rawPath := fs.String("raw", "agent.jsonl", "raw metric JSONL")
	agentID := fs.String("agent-id", "agent-001", "synthetic agent id")
	serverName := fs.String("server-name", "", "TLS server name; defaults to endpoint host")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	tlsConfig, err := loadClientTLS(*caPath, *endpoint, *serverName)
	if err != nil {
		return err
	}
	reg := metrics.NewRegistry()
	reg.Set(metrics.BufferBytes, 0)
	wcfg := workload.DefaultConfig()
	wcfg.AgentID = *agentID
	wcfg.AuditRecordsPerSecond = cfg.AuditRate
	wcfg.AuditPayloadBytes = cfg.AuditPayloadBytes
	wcfg.AuditMode = cfg.AuditMode
	wcfg.OperationDuration = cfg.OperationDuration()
	svc := workload.NewService(wcfg, reg)
	defer svc.Close()
	handler := contract.NewHandler(svc)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	session, err := connectAgent(ctx, *candidate, *endpoint, tlsConfig, transport.AgentID(*agentID), handler)
	if err != nil {
		return err
	}
	svc.BindSession(string(session.Info().SessionID))
	defer session.Close(nil)
	if err := os.MkdirAll(filepath.Dir(*rawPath), 0o700); err != nil {
		return err
	}
	raw, err := os.Create(*rawPath)
	if err != nil {
		return err
	}
	sampleCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	candidateCtx, stopCandidate := context.WithCancel(ctx)
	candidateDone := make(chan struct{})
	if source, ok := session.(interface {
		CandidateMetrics(context.Context) map[string]float64
	}); ok {
		copyCandidateMetrics(candidateCtx, source, reg)
		go func() {
			defer close(candidateDone)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-candidateCtx.Done():
					return
				case <-ticker.C:
					copyCandidateMetrics(candidateCtx, source, reg)
				}
			}
		}()
	} else {
		close(candidateDone)
	}
	go func() {
		done <- metrics.WriteJSONL(sampleCtx.Done(), raw, time.Second, reg, map[string]string{"role": "agent", "candidate": *candidate, "agent_id": *agentID})
	}()
	select {
	case <-ctx.Done():
	case <-session.Done():
	}
	stopCandidate()
	<-candidateDone
	if source, ok := session.(interface {
		CandidateMetrics(context.Context) map[string]float64
	}); ok {
		finalCtx, cancelFinal := context.WithTimeout(context.Background(), 500*time.Millisecond)
		copyCandidateMetrics(finalCtx, source, reg)
		cancelFinal()
	}
	stop()
	writeErr := <-done
	closeErr := raw.Close()
	return errors.Join(writeErr, closeErr)
}

func copyCandidateMetrics(ctx context.Context, source interface {
	CandidateMetrics(context.Context) map[string]float64
}, reg *metrics.Registry) {
	for name, value := range source.CandidateMetrics(ctx) {
		reg.Set(name, value)
	}
}

func runCert(args []string) error {
	fs := flag.NewFlagSet("cert", flag.ContinueOnError)
	output := fs.String("output", ".", "certificate output directory")
	hosts := fs.String("hosts", "127.0.0.1,localhost,dockpilot-server", "comma-separated IP/DNS SANs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cert, key, err := generateCertificate(*output, strings.Split(*hosts, ","))
	if err == nil {
		fmt.Printf("CERT %s\nKEY %s\n", cert, key)
	}
	return err
}

func newAcceptor(candidate string, listener net.Listener, tlsConfig *tls.Config, limits transport.Limits) (transport.Acceptor, error) {
	switch candidate {
	case "grpc":
		return grpcadapter.NewAcceptor(grpcadapter.AcceptorConfig{Listener: listener, TLSConfig: tlsConfig, Limits: limits}), nil
	case "websocket":
		return wsadapter.NewAcceptor(wsadapter.AcceptorConfig{Listener: listener, TLSConfig: tlsConfig, Limits: limits}), nil
	default:
		return nil, fmt.Errorf("unknown candidate %q", candidate)
	}
}

func connectAgent(ctx context.Context, candidate, endpoint string, tlsConfig *tls.Config, agentID transport.AgentID, handler transport.Handler) (transport.Session, error) {
	switch candidate {
	case "grpc":
		return grpcadapter.NewConnector(grpcadapter.ConnectorConfig{Address: endpoint, TLSConfig: tlsConfig, AgentID: agentID, Limits: transport.DefaultLimits()}).Connect(ctx, handler)
	case "websocket":
		return wsadapter.NewConnector(wsadapter.ConnectorConfig{URL: "wss://" + endpoint + wsadapter.Path, TLSConfig: tlsConfig, AgentID: agentID, Limits: transport.DefaultLimits()}).Connect(ctx, handler)
	default:
		return nil, fmt.Errorf("unknown candidate %q", candidate)
	}
}

func acceptAgents(ctx context.Context, acceptor transport.Acceptor, count int) ([]transport.Caller, error) {
	callers := make([]transport.Caller, 0, count)
	for len(callers) < count {
		caller, err := acceptor.Accept(ctx)
		if err != nil {
			closeCallers(callers)
			return nil, err
		}
		callers = append(callers, caller)
	}
	return callers, nil
}

func closeCallers(callers []transport.Caller) {
	for _, caller := range callers {
		_ = caller.Close(nil)
	}
}

func readConfig(path string) (experiment.Config, error) {
	var cfg experiment.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func generateCertificate(dir string, hosts []string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "dockpilot-transport-prototype"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	certPath := filepath.Join(dir, "prototype-cert.pem")
	keyPath := filepath.Join(dir, "prototype-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func loadServerTLS(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}, nil
}

func loadClientTLS(caPath, endpoint, override string) (*tls.Config, error) {
	b, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("CA file contains no certificate")
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, err
	}
	if override != "" {
		host = override
	}
	return &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS13}, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "transport-prototype: "+format+"\n", args...)
	os.Exit(1)
}
