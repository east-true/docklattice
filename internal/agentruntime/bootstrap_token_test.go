package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/east-true/docklattice/internal/identity"
)

// A Join Token is what gets an Agent registered. It is not what keeps a
// registered Agent running, and these tests pin that distinction at the only
// place it can be decided: the boot path.
//
// The failure they exist for is operational rather than theoretical. An Agent
// container's argument list is fixed at creation, so `--join-token-file` is
// still on it at every restart. Consuming the bootstrap secret and removing it
// - which is what a one-time secret invites - then made the Agent refuse to
// start, and a `restart: unless-stopped` policy could not recover it.

// countingSource stands in for a Join Token file. It records every resolution,
// which is how "the file was not read" is asserted without reasoning about
// syscalls: if the source was never called, nothing about the path was
// touched.
type countingSource struct {
	token string
	err   error
	calls int
}

func (s *countingSource) resolve() func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		s.calls++
		if s.err != nil {
			return "", s.err
		}
		return s.token, nil
	}
}

// bootstrapConfig moves the token off Config.JoinToken and onto a source, so
// that every test below can see whether it was consulted.
func bootstrapConfig(t *testing.T, root string, server *credentialServer, source *countingSource) Config {
	t.Helper()
	config := testConfig(root, server)
	config.JoinToken = ""
	config.JoinTokenSource = source.resolve()
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()
	return config
}

// TestRegisteredAgentRestartsWithoutItsJoinTokenFile is the regression this
// whole change exists for: enrol, delete the bootstrap secret, restart.
func TestRegisteredAgentRestartsWithoutItsJoinTokenFile(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	source := &countingSource{token: server.token}
	config := bootstrapConfig(t, root, server, source)

	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	firstID := first.Startup().AgentID
	snapshot, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Credential.Data) == 0 {
		t.Fatal("enrollment did not persist a runtime credential")
	}
	if source.calls != 1 {
		t.Fatalf("enrollment resolved the Join Token %d times, want exactly 1", source.calls)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The bootstrap secret is consumed and gone. The flag that points at it is
	// still on the command line, because a container's argument list does not
	// change between restarts.
	source.token = ""
	source.err = errors.New("inspect Join Token file: no such file or directory")

	second, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("a registered Agent refused to restart without its consumed Join Token file: %v", err)
	}
	defer second.Close(context.Background())

	if source.calls != 1 {
		t.Fatalf("restart consulted the Join Token source; calls = %d, want the enrollment's 1", source.calls)
	}
	if got := second.Startup().AgentID; got != firstID {
		t.Fatalf("Agent id changed across restart: %s -> %s", firstID, got)
	}
	if second.Startup().CurrentIncarnation != 2 {
		t.Fatalf("incarnation = %d, want 2", second.Startup().CurrentIncarnation)
	}
	if counts := server.counts(); counts.register != 1 {
		t.Fatalf("restart registered again: %+v", counts)
	}
}

// TestValidCredentialNeverResolvesTheJoinTokenSource states the rule directly,
// separately from the restart story: holding a usable credential is the
// condition, and no configured bootstrap source may be consulted under it.
func TestValidCredentialNeverResolvesTheJoinTokenSource(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	source := &countingSource{token: server.token}
	config := bootstrapConfig(t, root, server, source)

	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := source.calls

	// A source that fails loudly if touched at all. A restart that resolves it
	// would fail the boot rather than quietly reading a file it should not.
	touched := false
	config.JoinTokenSource = func(context.Context) (string, error) {
		touched = true
		return "", errors.New("the Join Token source must not be consulted by a registered Agent")
	}
	second, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("restart with a valid credential failed: %v", err)
	}
	defer second.Close(context.Background())
	if touched {
		t.Fatal("a registered Agent resolved its bootstrap Join Token source")
	}
	if source.calls != before {
		t.Fatalf("Join Token resolutions = %d, want %d", source.calls, before)
	}
}

// TestBootstrapWithoutAnyTokenSourceReportsWhatIsMissing covers the empty-state
// case: enrollment is genuinely required and there is nothing to enrol with.
// The refusal has to name that, not surface as an unrelated I/O error.
func TestBootstrapWithoutAnyTokenSourceReportsWhatIsMissing(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.JoinToken = ""
	config.JoinTokenSource = nil
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()

	_, err := Boot(context.Background(), config)
	if !errors.Is(err, ErrCredentialRequired) {
		t.Fatalf("boot error = %v, want ErrCredentialRequired", err)
	}
}

// TestBootstrapSurfacesAnUnreadableTokenSource is the other half of the fix. A
// missing file must stop being invisible *only* when nothing needs it; an
// Agent that has to enrol and cannot read its bootstrap secret still fails,
// and the reason still names the file rather than a generic refusal.
func TestBootstrapSurfacesAnUnreadableTokenSource(t *testing.T) {
	server := newCredentialServer(t)
	source := &countingSource{err: errors.New("inspect Join Token file: no such file or directory")}
	config := bootstrapConfig(t, t.TempDir(), server, source)

	_, err := Boot(context.Background(), config)
	if err == nil {
		t.Fatal("an Agent with no credential and an unreadable Join Token file booted")
	}
	if !strings.Contains(err.Error(), "Join Token file") {
		t.Fatalf("boot error = %v, want it to name the Join Token file", err)
	}
	if source.calls != 1 {
		t.Fatalf("Join Token resolutions = %d, want 1", source.calls)
	}
}

// TestBootstrapAcceptsAnInlineTokenOverASource keeps the existing contract:
// both input shapes are supported, and one already in hand wins without the
// source being consulted at all.
func TestBootstrapAcceptsAnInlineTokenOverASource(t *testing.T) {
	server := newCredentialServer(t)
	source := &countingSource{err: errors.New("the source must not be consulted when a token is already held")}
	config := bootstrapConfig(t, t.TempDir(), server, source)
	config.JoinToken = server.token

	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("boot with an inline Join Token: %v", err)
	}
	defer runtime.Close(context.Background())
	if source.calls != 0 {
		t.Fatalf("an inline Join Token still consulted the source %d times", source.calls)
	}
}

// TestARejectedCredentialDoesNotFallBackToEnrollment is the security half. A
// credential this Agent holds and the Server refuses is an authentication
// failure. Enrolling instead would let a Join Token that happens to still be
// on the command line walk straight around a revocation.
func TestARejectedCredentialDoesNotFallBackToEnrollment(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	source := &countingSource{token: server.token}
	config := bootstrapConfig(t, root, server, source)

	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := identity.ParseCredential(snapshot.Credential.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.manager.RevokeCredential(credential, server.now, "operator revoked"); err != nil {
		t.Fatal(err)
	}
	registerBefore := server.counts().register

	// Boot again with the bootstrap source still configured and working.
	second, err := Boot(context.Background(), config)
	if err == nil {
		defer second.Close(context.Background())
	}
	if counts := server.counts(); counts.register != registerBefore {
		t.Fatalf("a revoked credential triggered re-enrollment: register %d -> %d", registerBefore, counts.register)
	}
	if source.calls != 1 {
		t.Fatalf("a revoked credential resolved the Join Token source again; calls = %d, want the enrollment's 1", source.calls)
	}
	if err == nil {
		return
	}
	if errors.Is(err, ErrCredentialRequired) {
		t.Fatalf("a revoked credential was reported as a missing one: %v", err)
	}
}

// TestADifferentServerDoesNotTriggerEnrollment keeps the Server identity pin
// ahead of the bootstrap path. An Agent holding a valid credential from one
// Server, pointed at a different one with a working Join Token in reach, must
// not quietly join the new Server: that would make "any credential problem"
// into "enrol again somewhere else".
func TestADifferentServerDoesNotTriggerEnrollment(t *testing.T) {
	origin := newCredentialServer(t)
	root := t.TempDir()
	source := &countingSource{token: origin.token}
	config := bootstrapConfig(t, root, origin, source)

	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.Startup().AgentID
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A second Server, with its own identity and its own Join Token, which the
	// bootstrap source is perfectly able to supply.
	other := newCredentialServer(t)
	config.Registration = other.client
	source.token = other.token

	second, err := Boot(context.Background(), config)
	if err == nil {
		defer second.Close(context.Background())
		if got := second.Startup().AgentID; got != firstID {
			t.Fatalf("the Agent took a new identity from a different Server: %s -> %s", firstID, got)
		}
	}
	if counts := other.counts(); counts.register != 0 {
		t.Fatalf("the Agent enrolled with a different Server: %+v", counts)
	}
	if source.calls != 1 {
		t.Fatalf("a different Server caused the Join Token source to be resolved; calls = %d, want the original enrollment's 1", source.calls)
	}
}

// TestNoBootFailureCarriesTheJoinToken checks the leakage rule on the paths
// that produce operator-visible text. A boot error may name a file; it may
// never carry what the file contained.
func TestNoBootFailureCarriesTheJoinToken(t *testing.T) {
	server := newCredentialServer(t)
	secret := server.token
	if len(secret) < 8 {
		t.Fatalf("the fixture token is too short to be a meaningful leak probe: %q", secret)
	}
	source := &countingSource{token: secret}
	config := bootstrapConfig(t, t.TempDir(), server, source)
	// Registration is unreachable, so enrollment fails while holding the token.
	config.Registration = nil

	_, err := Boot(context.Background(), config)
	if err == nil {
		t.Fatal("boot succeeded with no registration client")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("a boot failure carried the Join Token: %v", err)
	}
}
