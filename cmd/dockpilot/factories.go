package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/east-true/dockpilot/internal/agentruntime"
	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/east-true/dockpilot/internal/app"
	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/registrationhttp"
	"github.com/east-true/dockpilot/internal/serverruntime"
)

func productFactories() app.Factories {
	return app.Factories{Server: func(cfg app.Config) (app.Runtime, error) {
		certificate := cfg.Server.TLSCertificateFile
		if certificate == "" {
			certificate = filepath.Join(cfg.Server.StateDir, "tls", "server.crt")
		}
		privateKey := cfg.Server.TLSPrivateKeyFile
		if privateKey == "" {
			privateKey = filepath.Join(cfg.Server.StateDir, "tls", "server.key")
		}
		return serverruntime.New(serverruntime.Config{
			StateDir:           cfg.Server.StateDir,
			HTTPListenAddress:  cfg.Server.ListenAddress,
			AgentListenAddress: cfg.Server.AgentListenAddress,
			TLSCertificateFile: certificate,
			TLSPrivateKeyFile:  privateKey,
			HeartbeatInterval:  cfg.Defaults.HeartbeatInterval,
			OfflineAfter:       cfg.Defaults.OfflineAfter,
			Diagnostics:        os.Stderr,
			AuditRetentionPolicy: auditstore.DefaultRetentionPolicy{
				MaxAge: cfg.Defaults.ServerAuditRetention, MaxBytes: cfg.Defaults.ServerAuditMaxBytes,
				WarningPercent: cfg.Defaults.ServerAuditWarnPercent, AggressivePercent: cfg.Defaults.ServerAuditAggressivePercent,
				LowPercent: cfg.Defaults.ServerAuditWarnPercent,
			},
		})
	}, Agent: newAgentProcess}
}

type agentProcess struct {
	config  agentruntime.Config
	runtime *agentruntime.Runtime
}

func newAgentProcess(cfg app.Config) (app.Runtime, error) {
	caPEM, err := os.ReadFile(cfg.Agent.ServerCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Server CA: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Server CA file contains no valid certificates")
	}
	host, _, err := net.SplitHostPort(cfg.Agent.ServerAddress)
	if err != nil || strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("invalid Server address %q", cfg.Agent.ServerAddress)
	}
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: strings.Trim(host, "[]"), MinVersion: tls.VersionTLS13}
	registration, err := registrationhttp.NewClient(cfg.Agent.RegistrationURL, &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig.Clone()},
		Timeout:   30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	joinToken := ""
	if cfg.Agent.JoinTokenFile != "" {
		joinToken, err = readJoinToken(cfg.Agent.JoinTokenFile)
		if err != nil {
			return nil, err
		}
	}
	return &agentProcess{config: agentruntime.Config{
		StateDir: filepath.Join(cfg.Agent.StateDir, "identity"), WALDir: filepath.Join(cfg.Agent.StateDir, "audit-wal"),
		Registration: registration, JoinToken: joinToken, DisplayName: cfg.Agent.DisplayName,
		ServerAddress: cfg.Agent.ServerAddress, TLSConfig: tlsConfig,
		Self:                  agentsafety.SelfConfig{ContainerID: cfg.Agent.SelfContainerID, ContainerName: cfg.Agent.SelfContainerName},
		ProjectRoots:          append([]string(nil), cfg.Agent.ProjectRoots...),
		BundledComposeVersion: "5.3.1",
		DiscoveryInterval:     cfg.Defaults.DiscoveryInterval,
		PeerSilenceTimeout:    cfg.Defaults.OfflineAfter,
	}}, nil
}

func readJoinToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect Join Token file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", errors.New("Join Token file must be a regular file with mode 0600")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open Join Token file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return "", errors.New("open Join Token file: invalid file descriptor")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("Join Token file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return "", fmt.Errorf("read Join Token file: %w", err)
	}
	if len(payload) > 64<<10 {
		clear(payload)
		return "", errors.New("Join Token file exceeds 64KiB")
	}
	token := strings.TrimSpace(string(payload))
	clear(payload)
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("Join Token file does not contain one token")
	}
	return token, nil
}

func (p *agentProcess) Ready(ctx context.Context) error {
	if p.runtime != nil {
		return nil
	}
	runtime, err := agentruntime.Boot(ctx, p.config)
	if err != nil {
		return err
	}
	p.runtime = runtime
	return nil
}

func (p *agentProcess) Run(ctx context.Context) error {
	if p.runtime == nil {
		return errors.New("Agent runtime is not ready")
	}
	runErr := p.runtime.Maintain(ctx)
	closeErr := p.runtime.Close(context.Background())
	return errors.Join(runErr, closeErr)
}
