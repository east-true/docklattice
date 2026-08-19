// Package serverruntime composes the product Server's durable foundations,
// Agent transport, registration API, and embedded browser UI.
package serverruntime

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/auditsync"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/registration"
	"github.com/east-true/dockpilot/internal/registrationhttp"
	"github.com/east-true/dockpilot/internal/serverapi"
	"github.com/east-true/dockpilot/internal/serverbootstrap"
	"github.com/east-true/dockpilot/internal/webui"
)

const acceptWorkers = 4

const livenessQueueSize = 1024

const (
	defaultAuditRetentionInterval = 15 * time.Minute
	defaultAuditRetentionTimeout  = time.Minute
)

type livenessObservation struct {
	agentID string
	at      time.Time
}

type Config struct {
	StateDir           string
	HTTPListenAddress  string
	AgentListenAddress string
	TLSCertificateFile string
	TLSPrivateKeyFile  string
	HeartbeatInterval  time.Duration
	OfflineAfter       time.Duration
	// AuditRetentionPolicy is evaluated only by the background retention
	// worker. It never participates in an Agent's ingest/ACK request path.
	AuditRetentionPolicy   auditstore.RetentionPolicy
	AuditRetentionInterval time.Duration
	AuditRetentionTimeout  time.Duration
	// Diagnostics receives one bounded line per rejected Agent admission. A
	// control plane that silently drops an Agent it cannot admit is not
	// operable, and admission is the one place where an Agent can disappear
	// with no trace in either the API or the Agent's own output.
	Diagnostics io.Writer
}

// admissionRejectionLogLimit bounds one diagnostic line so a hostile or looping
// peer cannot turn the Server's stderr into unbounded output.
const admissionRejectionLogLimit = 512

func (r *Runtime) logSessionClosed(agentID string, err error) {
	r.logDiagnostic("agent session closed", agentID, err)
}

func (r *Runtime) logAdmissionRejection(err error) {
	r.logDiagnostic("agent admission rejected", "", err)
}

func (r *Runtime) logDiagnostic(event, agentID string, err error) {
	if r.config.Diagnostics == nil || err == nil {
		return
	}
	message := err.Error()
	if len(message) > admissionRejectionLogLimit {
		message = message[:admissionRejectionLogLimit]
	}
	message = strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' {
			return ' '
		}
		return char
	}, message)
	if agentID != "" {
		fmt.Fprintf(r.config.Diagnostics, "%s agent=%s: %s\n", event, agentID, message)
		return
	}
	fmt.Fprintf(r.config.Diagnostics, "%s: %s\n", event, message)
}

type Runtime struct {
	config Config

	mu         sync.Mutex
	ready      bool
	running    bool
	components *serverbootstrap.Components
	httpServer *http.Server
	httpLn     net.Listener
	agentLn    net.Listener
	acceptor   *producttransport.ServerAcceptor
	registry   *producttransport.SessionRegistry
	liveness   chan livenessObservation
	audit      *auditsync.Server
	retention  auditRetentionStore
}

type auditRetentionStore interface {
	EnforceRetention(context.Context, string, auditstore.RetentionPolicy, time.Time) (auditstore.RetentionResult, error)
}

func New(config Config) (*Runtime, error) {
	if config.StateDir == "" || config.HTTPListenAddress == "" || config.AgentListenAddress == "" ||
		config.TLSCertificateFile == "" || config.TLSPrivateKeyFile == "" {
		return nil, errors.New("serverruntime: state dir, both listeners, and TLS certificate/key are required")
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = producttransport.DefaultHeartbeatInterval
	}
	if config.OfflineAfter <= 0 {
		config.OfflineAfter = producttransport.DefaultOfflineAfter
	}
	if config.OfflineAfter <= config.HeartbeatInterval {
		return nil, errors.New("serverruntime: offline threshold must exceed heartbeat interval")
	}
	if config.AuditRetentionPolicy == nil {
		config.AuditRetentionPolicy = auditstore.NewDefaultRetentionPolicy()
	}
	if config.AuditRetentionInterval <= 0 {
		config.AuditRetentionInterval = defaultAuditRetentionInterval
	}
	if config.AuditRetentionTimeout <= 0 {
		config.AuditRetentionTimeout = defaultAuditRetentionTimeout
	}
	return &Runtime{config: config}, nil
}

// Ready opens both independent Server stores, validates TLS material, builds
// all handlers, and binds both sockets. It returns only after no later startup
// step can silently turn the process into a successful no-op.
func (r *Runtime) Ready(ctx context.Context) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready {
		return nil
	}
	certificate, err := tls.LoadX509KeyPair(r.config.TLSCertificateFile, r.config.TLSPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("serverruntime: load TLS key pair: %w", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
	components, err := serverbootstrap.Open(ctx, r.config.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = components.Close()
		}
	}()

	registry := producttransport.NewSessionRegistryWithStore(restoringWatermarkStore{
		store: components.Store, now: func() time.Time { return time.Now().UTC() },
	})
	auditStore := auditstore.New(components.Store.DB())
	backend, err := serverapi.New(components.Store, registry,
		serverapi.WithAuditReadModel(components.Archive.AuditArchiveID, auditStore))
	if err != nil {
		return err
	}
	ui, err := webui.New(backend)
	if err != nil {
		return err
	}
	registrationService, err := registration.New(components.Store, components.Identity)
	if err != nil {
		return err
	}
	registrationHandler, err := registrationhttp.NewHandler(registrationService, components.Identity, registrationhttp.ArchiveIdentity{
		ServerIdentityID: components.Archive.ServerIdentityID,
		Generation:       components.Archive.Generation,
		AuditArchiveID:   components.Archive.AuditArchiveID,
	})
	if err != nil {
		return err
	}
	coverageStartReason := auditstore.CoverageServerNeverHad
	if components.Archive.Generation > 1 {
		coverageStartReason = auditstore.CoverageDatabaseReinitialized
	}
	auditServer, err := auditsync.NewServer(auditsync.ServerConfig{
		Store: auditStore, ArchiveID: components.Archive.AuditArchiveID,
		CoverageStartReason: coverageStartReason, Decoder: auditsync.CanonicalEventDecoder{},
		ServerIdentityID:  components.Archive.ServerIdentityID,
		ArchiveGeneration: components.Archive.Generation,
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/agent/", registrationHandler)
	mux.Handle("/", ui)
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	httpLn, err := net.Listen("tcp", r.config.HTTPListenAddress)
	if err != nil {
		return fmt.Errorf("serverruntime: bind HTTP listener: %w", err)
	}
	defer func() {
		if err != nil {
			_ = httpLn.Close()
		}
	}()
	agentLn, err := net.Listen("tcp", r.config.AgentListenAddress)
	if err != nil {
		return fmt.Errorf("serverruntime: bind Agent listener: %w", err)
	}
	defer func() {
		if err != nil {
			_ = agentLn.Close()
		}
	}()
	liveness := make(chan livenessObservation, livenessQueueSize)
	acceptor, err := producttransport.NewServerAcceptor(producttransport.ServerConfig{
		Listener: agentLn, TLSConfig: tlsConfig,
		Verifier: producttransport.IdentityVerifier{Manager: components.Identity},
		Registry: registry, HeartbeatInterval: r.config.HeartbeatInterval,
		OfflineAfter: r.config.OfflineAfter,
		LivenessObserver: func(info producttransport.SessionInfo, state producttransport.State, observationErr error) {
			if observationErr != nil || state != producttransport.StateActive {
				return
			}
			select {
			case liveness <- livenessObservation{agentID: info.AgentID, at: time.Now().UTC()}:
			default:
			}
		},
	})
	if err != nil {
		return err
	}

	r.components = components
	r.registry = registry
	r.httpServer = httpServer
	r.httpLn = tls.NewListener(httpLn, tlsConfig.Clone())
	r.agentLn = agentLn
	r.acceptor = acceptor
	r.liveness = liveness
	r.audit = auditServer
	r.retention = auditStore
	r.ready = true
	return nil
}

func (r *Runtime) Run(ctx context.Context) error {
	r.mu.Lock()
	if !r.ready || r.running {
		r.mu.Unlock()
		return errors.New("serverruntime: runtime is not ready or is already running")
	}
	r.running = true
	httpServer, httpLn, acceptor, liveness, store, auditServer, retentionStore, archiveID := r.httpServer, r.httpLn, r.acceptor, r.liveness, r.components.Store, r.audit, r.retention, r.components.Archive.AuditArchiveID
	retentionPolicy, retentionInterval, retentionTimeout := r.config.AuditRetentionPolicy, r.config.AuditRetentionInterval, r.config.AuditRetentionTimeout
	r.mu.Unlock()

	failures := make(chan error, acceptWorkers+1)
	livenessCtx, cancelLiveness := context.WithCancel(ctx)
	livenessDone := make(chan struct{})
	go func() {
		defer close(livenessDone)
		runLivenessWriter(livenessCtx, store, liveness)
	}()
	retentionCtx, cancelRetention := context.WithCancel(ctx)
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		runAuditRetentionWorker(retentionCtx, retentionStore, archiveID, retentionPolicy, retentionInterval, retentionTimeout)
	}()
	go func() {
		err := httpServer.Serve(httpLn)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			failures <- fmt.Errorf("serverruntime: serve HTTP: %w", err)
		}
	}()
	var workers sync.WaitGroup
	var auditWorkers sync.WaitGroup
	var sessions sync.Map
	for range acceptWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				session, err := acceptor.Accept(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if errors.Is(err, net.ErrClosed) {
						select {
						case failures <- fmt.Errorf("serverruntime: Agent listener closed: %w", err):
						default:
						}
						return
					}
					// A malformed or unauthenticated connection is isolated to that
					// connection and does not stop admission for other Agents.
					r.logAdmissionRejection(err)
					continue
				}
				sessionID := session.Info().SessionID
				sessions.Store(sessionID, session)
				auditSession, ok := session.(producttransport.AuditControlSession)
				if !ok {
					_ = session.Close(producttransport.ErrHandlerUnavailable)
					continue
				}
				auditWorkers.Add(1)
				go func() {
					defer auditWorkers.Done()
					if err := auditServer.Run(ctx, auditSession); err != nil && ctx.Err() == nil {
						r.logSessionClosed(session.Info().AgentID, err)
						_ = session.Close(err)
					}
				}()
				go func() {
					<-session.Done()
					sessions.Delete(sessionID)
				}()
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case runErr = <-failures:
	}
	_ = acceptor.Close()
	workers.Wait()
	sessions.Range(func(_, value any) bool {
		_ = value.(producttransport.ControlSession).Close(context.Canceled)
		return true
	})
	auditWorkers.Wait()
	cancelLiveness()
	<-livenessDone
	cancelRetention()
	<-retentionDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); runErr == nil && err != nil {
		runErr = err
	}
	if err := r.components.Close(); runErr == nil && err != nil {
		runErr = err
	}
	return runErr
}

// runAuditRetentionWorker is deliberately independent from Agent sessions:
// failed or timed-out retention work is retried on the next interval and never
// turns into an ingest/ACK error. EnforceRetention validates the active archive
// again inside each write transaction before it mutates canonical Audit data.
func runAuditRetentionWorker(ctx context.Context, store auditRetentionStore, archiveID string, policy auditstore.RetentionPolicy, interval, timeout time.Duration) {
	if store == nil || archiveID == "" || policy == nil || interval <= 0 || timeout <= 0 {
		return
	}
	runAuditRetentionOnce(ctx, store, archiveID, policy, timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runAuditRetentionOnce(ctx, store, archiveID, policy, timeout)
		}
	}
}

func runAuditRetentionOnce(ctx context.Context, store auditRetentionStore, archiveID string, policy auditstore.RetentionPolicy, timeout time.Duration) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = store.EnforceRetention(runCtx, archiveID, policy, time.Now().UTC())
}

type agentLastSeenStore interface {
	TouchAgentLastSeen(context.Context, string, time.Time) error
}

func runLivenessWriter(ctx context.Context, store agentLastSeenStore, observations <-chan livenessObservation) {
	for {
		select {
		case <-ctx.Done():
			return
		case observation := <-observations:
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = store.TouchAgentLastSeen(writeCtx, observation.agentID, observation.at)
			cancel()
		}
	}
}

// admissionStore supplies the session registry's incarnation watermark and
// restores the operational record of an Agent the transport has already
// authenticated but the Server no longer knows.
type admissionStore interface {
	producttransport.IncarnationWatermarkStore
	RestoreAuthenticatedAgent(context.Context, string, time.Time) error
}

// restoringWatermarkStore implements the outcome section 6.1 of the
// architecture assigns to losing the Audit database while the Identity State
// survives: existing Agents authenticate automatically. Signing keys and the
// revocation ledger live in the Identity State, so after a database loss the
// credential still verifies, but the agents row that carries the incarnation
// watermark is gone and admission would fail on the missing row. Restoring the
// record at that moment is what makes the automatic reconnection real, and a
// retired Agent is still refused because it is never revived.
type restoringWatermarkStore struct {
	store admissionStore
	now   func() time.Time
}

func (s restoringWatermarkStore) LoadIncarnation(ctx context.Context, agentID string) (uint64, error) {
	watermark, err := s.store.LoadIncarnation(ctx, agentID)
	if !errors.Is(err, sql.ErrNoRows) {
		return watermark, err
	}
	if restoreErr := s.store.RestoreAuthenticatedAgent(ctx, agentID, s.now()); restoreErr != nil {
		return 0, restoreErr
	}
	return s.store.LoadIncarnation(ctx, agentID)
}

func (s restoringWatermarkStore) CompareAndSwapIncarnation(ctx context.Context, agentID string, old, next uint64) (bool, error) {
	return s.store.CompareAndSwapIncarnation(ctx, agentID, old, next)
}

func (r *Runtime) HTTPAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.httpLn == nil {
		return ""
	}
	return r.httpLn.Addr().String()
}

func (r *Runtime) AgentAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agentLn == nil {
		return ""
	}
	return r.agentLn.Addr().String()
}
