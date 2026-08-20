package serverruntime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/auditsync"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/registration"
	"github.com/east-true/dockpilot/internal/serverstore"
)

func TestRuntimeServesEmbeddedUIAndStopsWithContext(t *testing.T) {
	certFile, keyFile, roots := testCertificate(t)
	runtime, err := New(Config{
		StateDir: secureStateDir(t), HTTPListenAddress: "127.0.0.1:0", AgentListenAddress: "127.0.0.1:0",
		TLSCertificateFile: certFile, TLSPrivateKeyFile: keyFile,
		HeartbeatInterval: 20 * time.Millisecond, OfflineAfter: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := runtime.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13,
	}}}
	response, err := client.Get("https://" + runtime.HTTPAddress() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if runtime.AgentAddress() == "" {
		t.Fatal("Agent listener address is empty")
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestReadyRejectsMissingTLSAndConflictingListener(t *testing.T) {
	runtime, err := New(Config{
		StateDir: secureStateDir(t), HTTPListenAddress: "127.0.0.1:0", AgentListenAddress: "127.0.0.1:0",
		TLSCertificateFile: "/missing/cert", TLSPrivateKeyFile: "/missing/key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Ready(context.Background()); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing TLS error = %v", err)
	}
}

type lastSeenCall struct {
	agentID string
	at      time.Time
}

type fakeLastSeenStore struct{ calls chan lastSeenCall }

func (s fakeLastSeenStore) TouchAgentLastSeen(_ context.Context, agentID string, at time.Time) error {
	s.calls <- lastSeenCall{agentID: agentID, at: at}
	return nil
}

// missingAgentStore reports an unknown Agent, which is what a Server sees after
// losing its database while keeping its Identity State.
type missingAgentStore struct {
	mu         sync.Mutex
	loads      []string
	restores   []string
	restored   bool
	restoreErr error
	swaps      int
}

func (s *missingAgentStore) LoadIncarnation(_ context.Context, agentID string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads = append(s.loads, agentID)
	if s.restored {
		return 7, nil
	}
	return 0, fmt.Errorf("serverstore: load Agent incarnation: %w", sql.ErrNoRows)
}

func (s *missingAgentStore) CompareAndSwapIncarnation(context.Context, string, uint64, uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swaps++
	return true, nil
}

func (s *missingAgentStore) RestoreAuthenticatedAgent(_ context.Context, agentID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restores = append(s.restores, agentID)
	if s.restoreErr != nil {
		return s.restoreErr
	}
	s.restored = true
	return nil
}

func (s *missingAgentStore) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.loads...), append([]string(nil), s.restores...)
}

func TestAdmissionRestoresAgentLostWithTheDatabase(t *testing.T) {
	store := &missingAgentStore{}
	watermark, err := restoringWatermarkStore{store: store, now: func() time.Time { return time.Unix(1, 0).UTC() }}.
		LoadIncarnation(context.Background(), "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if watermark != 7 {
		t.Fatalf("watermark after restore = %d", watermark)
	}
	loads, restores := store.snapshot()
	if len(restores) != 1 || restores[0] != "agent-a" {
		t.Fatalf("expected exactly one restore of agent-a, got %v", restores)
	}
	if len(loads) != 2 {
		t.Fatalf("expected the watermark to be reloaded after the restore, got %v", loads)
	}
}

func TestAdmissionFailsWhenTheAgentIsRetired(t *testing.T) {
	store := &missingAgentStore{restoreErr: serverstore.ErrAgentRetired}
	_, err := restoringWatermarkStore{store: store, now: func() time.Time { return time.Unix(1, 0).UTC() }}.
		LoadIncarnation(context.Background(), "agent-a")
	if !errors.Is(err, serverstore.ErrAgentRetired) {
		t.Fatalf("retired admission error = %v", err)
	}
	loads, restores := store.snapshot()
	if len(restores) != 1 || len(loads) != 1 {
		t.Fatalf("a refused restore must not be retried: loads=%v restores=%v", loads, restores)
	}
}

func TestLivenessWriterPersistsBoundedObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := fakeLastSeenStore{calls: make(chan lastSeenCall, 1)}
	observations := make(chan livenessObservation, 1)
	go runLivenessWriter(ctx, store, observations)
	want := livenessObservation{agentID: "agent-1", at: time.Now().UTC()}
	observations <- want
	select {
	case got := <-store.calls:
		if got.agentID != want.agentID || !got.at.Equal(want.at) {
			t.Fatalf("last_seen call = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("liveness writer did not persist observation")
	}
}

type auditRetentionCall struct {
	archiveID string
	policy    auditstore.RetentionPolicy
	bounded   bool
}

type blockingAuditRetentionStore struct{ calls chan auditRetentionCall }

func (store blockingAuditRetentionStore) EnforceRetention(ctx context.Context, archiveID string, policy auditstore.RetentionPolicy, _ time.Time) (auditstore.RetentionResult, error) {
	_, bounded := ctx.Deadline()
	select {
	case store.calls <- auditRetentionCall{archiveID: archiveID, policy: policy, bounded: bounded}:
	case <-ctx.Done():
		return auditstore.RetentionResult{}, ctx.Err()
	}
	<-ctx.Done()
	return auditstore.RetentionResult{}, ctx.Err()
}

type failingAuditRetentionStore struct{ calls chan auditRetentionCall }

func (store failingAuditRetentionStore) EnforceRetention(ctx context.Context, archiveID string, policy auditstore.RetentionPolicy, _ time.Time) (auditstore.RetentionResult, error) {
	_, bounded := ctx.Deadline()
	select {
	case store.calls <- auditRetentionCall{archiveID: archiveID, policy: policy, bounded: bounded}:
	case <-ctx.Done():
		return auditstore.RetentionResult{}, ctx.Err()
	}
	return auditstore.RetentionResult{}, errors.New("injected retention failure")
}

func TestAuditRetentionWorkerRetriesWithoutFailingRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := failingAuditRetentionStore{calls: make(chan auditRetentionCall, 3)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAuditRetentionWorker(ctx, store, "current-archive", auditstore.NewDefaultRetentionPolicy(), 10*time.Millisecond, time.Second)
	}()
	for attempt := 0; attempt < 2; attempt++ {
		select {
		case call := <-store.calls:
			if call.archiveID != "current-archive" || !call.bounded {
				t.Fatalf("retention call = %#v", call)
			}
		case <-time.After(time.Second):
			t.Fatal("retention worker did not retry after a failure")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention worker did not stop with runtime context")
	}
}

type auditRuntimeHandler struct{ audit *auditsync.Agent }

func (*auditRuntimeHandler) Heartbeat(context.Context, producttransport.SessionInfo, time.Time) (producttransport.Capability, error) {
	return producttransport.Capability{ConnectionReady: true}, nil
}
func (h *auditRuntimeHandler) SyncAudit(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditSyncStream) error {
	return h.audit.SyncAudit(ctx, info, stream)
}

func TestRuntimeIngestsAuditOverAuthenticatedReverseGRPC(t *testing.T) {
	certFile, keyFile, roots := testCertificate(t)
	runtime, err := New(Config{
		StateDir: secureStateDir(t), HTTPListenAddress: "127.0.0.1:0", AgentListenAddress: "127.0.0.1:0",
		TLSCertificateFile: certFile, TLSPrivateKeyFile: keyFile,
		HeartbeatInterval: time.Hour, OfflineAfter: 2 * time.Hour,
		AuditRetentionInterval: time.Hour, AuditRetentionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := runtime.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	retention := blockingAuditRetentionStore{calls: make(chan auditRetentionCall, 1)}
	runtime.retention = retention
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(ctx) }()
	select {
	case call := <-retention.calls:
		if call.archiveID != runtime.components.Archive.AuditArchiveID {
			t.Fatalf("retention archive = %q, want active archive %q", call.archiveID, runtime.components.Archive.AuditArchiveID)
		}
		if !call.bounded {
			t.Fatal("retention run was not bounded by a deadline")
		}
		if _, ok := call.policy.(auditstore.DefaultRetentionPolicy); !ok {
			t.Fatalf("retention policy = %T, want default policy", call.policy)
		}
	case <-time.After(time.Second):
		t.Fatal("retention worker did not start")
	}

	agentID, err := agentid.New()
	if err != nil {
		t.Fatal(err)
	}
	registrationService, err := registration.New(runtime.components.Store, runtime.components.Identity)
	if err != nil {
		t.Fatal(err)
	}
	token, err := registrationService.IssueJoinToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := registrationService.Register(context.Background(), registration.Request{
		JoinToken: token.Token, AgentID: agentID, DisplayName: "Audit Agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := identity.MarshalCredential(registered.Credential)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(credential)

	wal, err := auditwal.Open(filepath.Join(t.TempDir(), "wal"), agentID, 1, auditwal.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	if err := wal.RebindArchive(runtime.components.Archive.AuditArchiveID); err != nil {
		t.Fatal(err)
	}
	payload, err := auditevents.EncodeManaged(auditgen.Signal{
		ResourceType: "container", ResourceID: "container-1", Action: "restart", OccurredAt: time.Now().UTC(),
	}, "ui:127.0.0.1", "project-1", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	auditAgent, err := auditsync.NewAgent(auditsync.AgentConfig{WAL: wal, ArchiveID: runtime.components.Archive.AuditArchiveID, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	connector, err := producttransport.NewAgentConnector(producttransport.AgentConfig{
		Address: runtime.AgentAddress(), TLSConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13},
		Credential: credential, Incarnation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := connector.Connect(ctx, &auditRuntimeHandler{audit: auditAgent})
	if err != nil {
		t.Fatal(err)
	}
	defer agentSession.Close(nil)

	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		err := runtime.components.Store.DB().QueryRowContext(context.Background(), `
			SELECT count(*) FROM audit_events
			WHERE agent_id=? AND incarnation=1 AND seq=1 AND operation_id='operation-1'
		`, agentID).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		bounds, boundsErr := wal.Bounds()
		if count == 1 && boundsErr == nil && bounds.ServerACKedThrough != nil && bounds.ServerACKedThrough.Seq == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Audit did not sync: count=%d bounds=%#v err=%v", count, bounds, boundsErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = agentSession.Close(nil)
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
}

func testCertificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("append certificate")
	}
	return certFile, keyFile, roots
}

// secureStateDir returns a temporary directory the Server may adopt. t.TempDir
// inherits the umask the suite was launched with, and a state directory that
// is group- or other-writable is refused by design, so the mode is made
// explicit here rather than depending on how the suite was invoked.
func secureStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
