package registrationhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/registration"
	"github.com/east-true/dockpilot/internal/serverstore"
)

type fixture struct {
	handler    *Handler
	service    *registration.Service
	identities *identity.Manager
	client     *Client
	server     *httptest.Server
	now        time.Time
	archive    ArchiveIdentity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	identities, err := identity.Open(filepath.Join(root, "identity", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := serverstore.Open(ctx, filepath.Join(root, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := registration.New(store, identities)
	if err != nil {
		t.Fatal(err)
	}
	archive := ArchiveIdentity{ServerIdentityID: identities.ServerIdentityID(), Generation: 1, AuditArchiveID: strings.Repeat("a", 32)}
	handler, err := NewHandler(service, identities, archive)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	handler.now = func() time.Time { return now }
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	client, err := NewClient(server.URL, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{handler: handler, service: service, identities: identities, client: client, server: server, now: now, archive: archive}
}

func TestRegisterRenewSaveActivateOrdering(t *testing.T) {
	fixture := newFixture(t)
	fixture.serviceNow(t, fixture.now)
	token, err := fixture.service.IssueJoinToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := agentid.New()
	if err != nil {
		t.Fatal(err)
	}
	registered, err := fixture.client.Register(context.Background(), RegisterRequest{
		JoinToken: token.Token, AgentID: agentID, DisplayName: "host-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Archive != fixture.archive {
		t.Fatalf("archive = %#v", registered.Archive)
	}

	// Anchor the lifecycle check to the credential's issuance time rather than
	// the historical fixture date; this keeps the renewal threshold stable as
	// the test suite is run in later calendar years.
	fixture.handler.now = func() time.Time { return registered.Credential.IssuedAt.Add(46 * 24 * time.Hour) }
	renewed, err := fixture.client.Renew(context.Background(), RenewRequest{Current: registered.Credential})
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(secureStateDir(t), "credential.json")
	if err := identity.SaveCredential(credentialPath, renewed.Credential); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Activate(context.Background(), ActivateRequest{Previous: registered.Credential, Active: renewed.Credential}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.identities.VerifyCredential(registered.Credential, registered.Credential.IssuedAt.Add(46*24*time.Hour)); !errors.Is(err, identity.ErrRevokedCredential) {
		t.Fatalf("previous credential after activation = %v", err)
	}
	if err := fixture.identities.VerifyCredential(renewed.Credential, registered.Credential.IssuedAt.Add(46*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) serviceNow(t *testing.T, now time.Time) {
	t.Helper()
	// registration.Service intentionally keeps its clock private. Issue the
	// short-lived token with real time and use the handler clock only for the
	// credential lifecycle; this method documents that separation.
	_ = now
}

func TestRegisterRejectsBadTokenWithoutEchoingSecret(t *testing.T) {
	fixture := newFixture(t)
	agentID, _ := agentid.New()
	request, err := http.NewRequest(http.MethodPost, fixture.server.URL+RegisterPath,
		strings.NewReader(`{"join_token":"do-not-echo","agent_id":"`+agentID+`","display_name":"host"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestHandlerRejectsPlainHTTPAndEarlyRenewal(t *testing.T) {
	fixture := newFixture(t)
	plain := httptest.NewRecorder()
	fixture.handler.ServeHTTP(plain, httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(`{}`)))
	if plain.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain HTTP status = %d", plain.Code)
	}

	token, err := fixture.service.IssueJoinToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := agentid.New()
	registered, err := fixture.client.Register(context.Background(), RegisterRequest{
		JoinToken: token.Token, AgentID: agentID, DisplayName: "host",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler.now = time.Now
	_, err = fixture.client.Renew(context.Background(), RenewRequest{Current: registered.Credential})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("early renewal error = %v", err)
	}
}

func TestClientRequiresHTTPSAndRejectsIdentityMismatch(t *testing.T) {
	if _, err := NewClient("http://example.test", http.DefaultClient); err == nil {
		t.Fatal("plain HTTP client accepted")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, CredentialResponse{
			Credential: identity.Credential{AgentID: "another", CredentialID: "new"},
			Archive:    ArchiveIdentity{ServerIdentityID: "server", Generation: 1, AuditArchiveID: "archive"},
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Register(context.Background(), RegisterRequest{AgentID: "expected"})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
}

func TestTLSIdentityIsVerifiedByConfiguredHTTPClient(t *testing.T) {
	fixture := newFixture(t)
	untrusted := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}}
	client, err := NewClient(fixture.server.URL, untrusted)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Register(context.Background(), RegisterRequest{})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted TLS error = %v", err)
	}
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
