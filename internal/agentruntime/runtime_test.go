package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/agentsafety"
	"github.com/east-true/docklattice/internal/agentstate"
	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditgen"
	"github.com/east-true/docklattice/internal/auditwal"
	"github.com/east-true/docklattice/internal/dockeradapter"
	"github.com/east-true/docklattice/internal/identity"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/registrationhttp"
)

func TestBootRegistersOverTLSAndCleanRestartPreservesIdentity(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	config := testConfig(root, server)
	docker := &fakeDocker{containers: labelledSelf()}
	config.DockerOpen = docker.opener()

	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if first.Startup().CurrentIncarnation != 1 || firstSnapshot.BoundArchive == nil ||
		firstSnapshot.BoundArchive.ArchiveID != server.archive.AuditArchiveID || len(firstSnapshot.Credential.Data) == 0 {
		t.Fatalf("first boot state = %+v startup=%+v", firstSnapshot, first.Startup())
	}
	if _, ok := any(first.handler).(producttransport.QueryHandler); ok {
		t.Fatal("placeholder heartbeat handler unexpectedly implements Query")
	}
	if _, ok := any(first.handler).(producttransport.AuditSyncHandler); !ok {
		t.Fatal("booted Agent did not wire durable Audit Sync")
	}
	capability, err := first.handler.Heartbeat(context.Background(), producttransport.SessionInfo{
		AgentID: first.Startup().AgentID, Incarnation: 1,
		CredentialID: first.credential.CredentialID, ServerIdentityID: first.credential.ServerIdentityID,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !capability.ConnectionReady || !capability.DockerReady || capability.ComposeReady {
		t.Fatalf("capability = %+v", capability)
	}
	if _, err := first.WAL().Append(context.Background(), []byte(`{"kind":"boot"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	config.JoinToken = ""
	second, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if second.Startup().AgentID != first.Startup().AgentID || second.Startup().CurrentIncarnation != 2 || second.Startup().PreviousUnclean {
		t.Fatalf("restart startup = %+v, first = %+v", second.Startup(), first.Startup())
	}
	if server.counts().register != 1 {
		t.Fatalf("registration calls = %+v", server.counts())
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	inspection, err := agentstate.Inspect(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Exists || inspection.AgentID != first.Startup().AgentID || inspection.CurrentIncarnation != 2 {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func TestRenewalPersistsThenActivatesAndResumesAfterFailure(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	config := testConfig(root, server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()
	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	previous, err := identity.ParseCredential(snapshot.Credential.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	future := previous.IssuedAt.Add(identity.CredentialLifetime/2 + time.Hour)
	server.setNow(future)
	server.failNextActivation()
	config.JoinToken = ""
	config.Now = func() time.Time { return future }
	if _, err := Boot(context.Background(), config); err == nil {
		t.Fatal("Boot succeeded despite failed activation")
	}

	// The replacement and previous credential activation journal survived the
	// failed boot. Restart retries Activate before connecting.
	recovered, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	recoveredSnapshot, err := recovered.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if recoveredSnapshot.PendingActivation != nil {
		t.Fatalf("pending activation = %+v", recoveredSnapshot.PendingActivation)
	}
	active, err := identity.ParseCredential(recoveredSnapshot.Credential.Data)
	if err != nil {
		t.Fatal(err)
	}
	if active.CredentialID == previous.CredentialID {
		t.Fatal("renewal kept previous credential")
	}
	if err := server.manager.VerifyCredential(previous, future); !errors.Is(err, identity.ErrRevokedCredential) {
		t.Fatalf("previous credential after resumed activation = %v", err)
	}
	counts := server.counts()
	if counts.renew != 1 || counts.activate != 2 {
		t.Fatalf("credential calls = %+v", counts)
	}
	if err := recovered.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSelfIdentificationFailureIsFailClosedInHeartbeat(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	docker := &fakeDocker{containers: []dockeradapter.Container{{ID: "unrelated", Names: []string{"other"}}}}
	config.DockerOpen = docker.opener()
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	capability, err := runtime.handler.Heartbeat(context.Background(), producttransport.SessionInfo{
		AgentID: runtime.Startup().AgentID, Incarnation: runtime.Startup().CurrentIncarnation,
		CredentialID: runtime.credential.CredentialID, ServerIdentityID: runtime.credential.ServerIdentityID,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if capability.DockerReady || capability.ComposeReady || capability.Reason == "" {
		t.Fatalf("fail-closed capability = %+v", capability)
	}
	if identification := docker.identification(); !identification.FailClosed {
		t.Fatalf("Docker mutation identity = %+v", identification)
	}
}

func TestObservedDockerEventDrainsToWALAndPersistsCheckpoint(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	events := make(chan dockeradapter.Event, 1)
	errorsIn := make(chan error)
	docker := &fakeEventDocker{
		fakeDocker: &fakeDocker{containers: labelledSelf()},
		first:      dockeradapter.EventStream{Events: events, Errors: errorsIn},
	}
	config.DockerOpen = docker.opener()
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	at := server.now.Add(time.Minute)
	events <- dockeradapter.Event{ResourceType: "container", ResourceID: "workload", Action: "start", OccurredAt: at}
	close(events)
	close(errorsIn)

	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, snapshotErr := runtime.Snapshot()
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if snapshot.LastDockerEventAt.Equal(at) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Docker event checkpoint = %s, want %s", snapshot.LastDockerEventAt, at)
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, err := runtime.WAL().ReadAuditFrom(context.Background(), auditwal.Cursor{
		Incarnation: runtime.Startup().CurrentIncarnation, Seq: 1,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range result.Records {
		envelope, decodeErr := auditevents.Decode(record.Payload)
		if decodeErr == nil && envelope.Event.Kind == auditgen.KindObserved && envelope.Event.ResourceID == "workload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("observed event was not present in WAL: %+v", result.Records)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUncleanRestartAppendsContinuityBoundary(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()
	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WAL().Append(context.Background(), []byte(`{"kind":"pre-crash"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.wal.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := first.wal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closeDocker(first.docker); err != nil {
		t.Fatal(err)
	}
	if err := first.state.Close(); err != nil {
		t.Fatal(err)
	}

	config.JoinToken = ""
	second, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Startup().PreviousUnclean || second.Startup().PreviousIncarnation != 1 {
		t.Fatalf("startup = %+v", second.Startup())
	}
	result, err := second.WAL().ReadAuditFrom(context.Background(), auditwal.Cursor{Incarnation: 1, Seq: 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range result.Records {
		envelope, decodeErr := auditevents.Decode(record.Payload)
		if decodeErr == nil && envelope.Event.Kind == auditevents.KindContinuityUncertain {
			found = envelope.PreviousIncarnation == 1 && envelope.KnownDurableThrough != nil && *envelope.KnownDurableThrough == 1
		}
	}
	if !found {
		t.Fatalf("continuity boundary not found: %+v", result.Records)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDockerSnapshotReconciliationEmitsOnlyDigestAndOneAudit(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	docker := &fakeDocker{containers: labelledSelf()}
	config.DockerOpen = docker.opener()
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileDockerSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Snapshot()
	if err != nil || len(before.DockerSnapshotSHA256) != 64 {
		t.Fatalf("initial snapshot = %+v, %v", before, err)
	}
	docker.mu.Lock()
	docker.containers = append(docker.containers, dockeradapter.Container{
		ID: strings.Repeat("c", 64), Image: "private.registry/workload:latest", State: "running",
	})
	docker.mu.Unlock()
	if err := runtime.reconcileDockerSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Snapshot()
	if err != nil || after.DockerSnapshotSHA256 == before.DockerSnapshotSHA256 {
		t.Fatalf("updated snapshot = %+v, %v", after, err)
	}
	result, err := runtime.WAL().ReadAuditFrom(context.Background(), auditwal.Cursor{
		Incarnation: runtime.Startup().CurrentIncarnation, Seq: 1,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range result.Records {
		envelope, decodeErr := auditevents.Decode(record.Payload)
		if decodeErr == nil && envelope.Event.Action == "unobserved_change" {
			count++
			if strings.Contains(string(record.Payload), "private.registry") || envelope.Event.ResourceID != "inventory" {
				t.Fatalf("snapshot Audit leaked Docker state: %s", record.Payload)
			}
		}
	}
	if count != 1 {
		t.Fatalf("snapshot reconciliation Audit count = %d", count)
	}
	if err := runtime.reconcileDockerSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainReconnectUsesDurableCredentialAndCancellationCleansSession(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()
	connected := make(chan *fakeOutboundSession, 1)
	config.Connect = func(_ context.Context, payload []byte, incarnation uint64, handler producttransport.AgentHandler) (producttransport.Session, error) {
		credential, err := identity.ParseCredential(payload)
		if err != nil {
			return nil, err
		}
		if incarnation == 0 || credential.AgentID == "" || handler == nil {
			return nil, errors.New("invalid connect inputs")
		}
		session := newFakeOutboundSession(credential.AgentID, incarnation)
		connected <- session
		return session, nil
	}
	config.ReconnectPolicy = producttransport.ReconnectPolicy{Initial: time.Millisecond, Maximum: time.Millisecond, Multiplier: 1}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Maintain(context.Background()) }()
	session := <-connected

	const closers = 8
	var wait sync.WaitGroup
	errs := make(chan error, closers)
	for i := 0; i < closers; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); errs <- runtime.Close(context.Background()) }()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("outbound session was not closed by Runtime.Close")
	}
}

type fakeDocker struct {
	mu         sync.Mutex
	provider   dockeradapter.IdentityProvider
	containers []dockeradapter.Container
	probe      dockeradapter.Capability
	probeErr   error
	closed     int
}

type fakeEventDocker struct {
	*fakeDocker
	mu    sync.Mutex
	first dockeradapter.EventStream
	used  bool
}

func (docker *fakeEventDocker) opener() DockerOpenFunc {
	return func(provider dockeradapter.IdentityProvider) (Docker, error) {
		docker.fakeDocker.mu.Lock()
		docker.fakeDocker.provider = provider
		docker.fakeDocker.mu.Unlock()
		return docker, nil
	}
}

func (docker *fakeEventDocker) SubscribeEvents(ctx context.Context, _ time.Time) (dockeradapter.EventStream, error) {
	docker.mu.Lock()
	defer docker.mu.Unlock()
	if !docker.used {
		docker.used = true
		return docker.first, nil
	}
	events := make(chan dockeradapter.Event)
	errorsIn := make(chan error)
	go func() {
		<-ctx.Done()
		close(events)
		close(errorsIn)
	}()
	return dockeradapter.EventStream{Events: events, Errors: errorsIn}, nil
}

func (docker *fakeEventDocker) Inspect(context.Context, string) (dockeradapter.Container, error) {
	return dockeradapter.Container{}, nil
}

func (d *fakeDocker) opener() DockerOpenFunc {
	return func(provider dockeradapter.IdentityProvider) (Docker, error) {
		d.mu.Lock()
		d.provider = provider
		d.mu.Unlock()
		return d, nil
	}
}
func (d *fakeDocker) Probe(context.Context) (dockeradapter.Capability, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	capability := d.probe
	if capability == (dockeradapter.Capability{}) && d.probeErr == nil {
		capability = dockeradapter.Capability{Available: true, ServerAPIVersion: "1.55", EngineVersion: "29.0"}
	}
	return capability, d.probeErr
}
func (d *fakeDocker) List(context.Context) ([]dockeradapter.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]dockeradapter.Container, len(d.containers))
	copy(result, d.containers)
	return result, nil
}
func (d *fakeDocker) Close() error { d.mu.Lock(); d.closed++; d.mu.Unlock(); return nil }
func (d *fakeDocker) identification() agentsafety.Identification {
	d.mu.Lock()
	provider := d.provider
	d.mu.Unlock()
	if provider == nil {
		return agentsafety.Identification{}
	}
	return provider()
}

func labelledSelf() []dockeradapter.Container {
	return []dockeradapter.Container{{
		ID: "agent-container", Names: []string{"docklattice-agent"},
		Labels: map[string]string{agentsafety.AgentRoleLabel: agentsafety.AgentRoleValue, agentsafety.ComposeProjectLabel: "docklattice"},
	}}
}

type fakeOutboundSession struct {
	info producttransport.SessionInfo
	done chan struct{}
	once sync.Once
}

func newFakeOutboundSession(agentID string, incarnation uint64) *fakeOutboundSession {
	return &fakeOutboundSession{info: producttransport.SessionInfo{AgentID: agentID, Incarnation: incarnation, SessionID: "outbound"}, done: make(chan struct{})}
}
func (s *fakeOutboundSession) Info() producttransport.SessionInfo { return s.info }
func (s *fakeOutboundSession) Done() <-chan struct{}              { return s.done }
func (s *fakeOutboundSession) Err() error                         { return nil }
func (s *fakeOutboundSession) Close(error) error                  { s.once.Do(func() { close(s.done) }); return nil }

type credentialCounts struct{ register, renew, activate int }

type credentialServer struct {
	mu             sync.Mutex
	manager        *identity.Manager
	server         *httptest.Server
	client         *registrationhttp.Client
	archive        registrationhttp.ArchiveIdentity
	now            time.Time
	token          string
	countsValue    credentialCounts
	failActivation bool
}

func newCredentialServer(t *testing.T) *credentialServer {
	t.Helper()
	manager, err := identity.Open(filepath.Join(t.TempDir(), "server-identity", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &credentialServer{
		manager: manager, now: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), token: "one-time-test-token",
		archive: registrationhttp.ArchiveIdentity{ServerIdentityID: manager.ServerIdentityID(), Generation: 1, AuditArchiveID: "archive-1"},
	}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	httpClient := fixture.server.Client()
	httpClient.Timeout = 2 * time.Second
	fixture.client, err = registrationhttp.NewClient(fixture.server.URL, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (s *credentialServer) serveHTTP(w http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case registrationhttp.RegisterPath:
		var value registrationhttp.RegisterRequest
		if json.NewDecoder(request.Body).Decode(&value) != nil || value.JoinToken != s.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		credential, err := s.manager.IssueCredential(value.AgentID, s.now)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.countsValue.register++
		writeCredentialResponse(w, http.StatusCreated, credential, s.archive)
	case registrationhttp.RenewPath:
		var value registrationhttp.RenewRequest
		if json.NewDecoder(request.Body).Decode(&value) != nil || !value.Current.RenewalDue(s.now) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		credential, err := s.manager.RenewCredential(value.Current, s.now)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.countsValue.renew++
		writeCredentialResponse(w, http.StatusOK, credential, s.archive)
	case registrationhttp.ActivatePath:
		s.countsValue.activate++
		if s.failActivation {
			s.failActivation = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var value registrationhttp.ActivateRequest
		if json.NewDecoder(request.Body).Decode(&value) != nil || s.manager.VerifyCredential(value.Active, s.now) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := s.manager.RevokeCredential(value.Previous, s.now, "replacement activated"); err != nil && !errors.Is(err, identity.ErrExpiredCredential) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(registrationhttp.ActivateResponse{Activated: true})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeCredentialResponse(w http.ResponseWriter, status int, credential identity.Credential, archive registrationhttp.ArchiveIdentity) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(registrationhttp.CredentialResponse{Credential: credential, Archive: archive})
}
func (s *credentialServer) setNow(now time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }
func (s *credentialServer) failNextActivation()  { s.mu.Lock(); s.failActivation = true; s.mu.Unlock() }
func (s *credentialServer) counts() credentialCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countsValue
}

func testConfig(root string, server *credentialServer) Config {
	return Config{
		StateDir: filepath.Join(root, "state"), WALDir: filepath.Join(root, "wal"),
		Registration: server.client, JoinToken: server.token, DisplayName: "test-host",
		Connect: func(context.Context, []byte, uint64, producttransport.AgentHandler) (producttransport.Session, error) {
			return nil, errors.New("not connected during boot test")
		},
		Now: func() time.Time { server.mu.Lock(); defer server.mu.Unlock(); return server.now },
	}
}
