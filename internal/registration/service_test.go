package registration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
	"github.com/east-true/dockpilot/internal/agentstate"
	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/serverstore"
)

var fixedNow = time.Date(2026, 8, 15, 9, 0, 0, 123456789, time.FixedZone("KST", 9*60*60))

func TestIssueJoinTokenReturns256BitSecretAndStoresOnlyHash(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	issued, err := service.IssueJoinToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, secret, kind, bound, err := parsePresentedToken(issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if id != issued.ID || kind != "join" || bound != "" || len(secret) != JoinTokenSecretBytes {
		t.Fatalf("bad issued token: id=%q kind=%q bound=%q secret=%d", id, kind, bound, len(secret))
	}
	if got := strings.Count(issued.Token, "."); got != 1 {
		t.Fatalf("token separator count = %d", got)
	}
	if !issued.ExpiresAt.Equal(canonicalTime(fixedNow).Add(time.Hour)) {
		t.Fatalf("expiry = %s", issued.ExpiresAt)
	}
	var storedID string
	var storedHash []byte
	if err := service.store.DB().QueryRowContext(ctx, "SELECT id, hash FROM join_tokens WHERE id = ?", issued.ID).Scan(&storedID, &storedHash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(secret)
	if storedID != issued.ID || len(storedHash) != sha256.Size || string(storedHash) != string(digest[:]) {
		t.Fatalf("stored token material is not SHA-256 only: id=%q hash_len=%d", storedID, len(storedHash))
	}
	if string(storedHash) == string(secret) {
		t.Fatal("database stored plaintext secret")
	}
	columns := joinTokenColumns(t, ctx, service)
	for _, column := range columns {
		if strings.Contains(strings.ToLower(column), "secret") || strings.Contains(strings.ToLower(column), "token") && column != "id" {
			t.Fatalf("plaintext-capable token column present: %q", column)
		}
	}
}

func TestNewRegistrationConsumesTokenCreatesAgentAndCredential(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	issued := mustIssue(t, ctx, service, time.Hour)
	agentID := mustAgentID(t)
	result, err := service.Register(ctx, Request{
		JoinToken: issued.Token, AgentID: agentID, DisplayName: "lab host", Metadata: map[string]string{"zone": "seoul", "owner": "ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentID != agentID || !agentid.Valid(result.AgentID) || result.Credential.AgentID != result.AgentID {
		t.Fatalf("bad registration result: %+v", result)
	}
	if result.Credential.ExpiresAt.Sub(result.Credential.IssuedAt) != identity.CredentialLifetime {
		t.Fatalf("credential lifetime = %s", result.Credential.ExpiresAt.Sub(result.Credential.IssuedAt))
	}
	if err := service.identities.VerifyCredential(result.Credential, canonicalTime(fixedNow)); err != nil {
		t.Fatalf("issued credential invalid: %v", err)
	}
	var displayName, metadata, consumedAt string
	if err := service.store.DB().QueryRowContext(ctx, "SELECT display_name, metadata_json FROM agents WHERE id = ?", result.AgentID).Scan(&displayName, &metadata); err != nil {
		t.Fatal(err)
	}
	if displayName != "lab host" || !strings.Contains(metadata, `"zone":"seoul"`) || !strings.Contains(metadata, `"owner":"ops"`) {
		t.Fatalf("agent row = display=%q metadata=%q", displayName, metadata)
	}
	if err := service.store.DB().QueryRowContext(ctx, "SELECT consumed_at FROM join_tokens WHERE id = ?", issued.ID).Scan(&consumedAt); err != nil || consumedAt == "" {
		t.Fatalf("token consumed_at = %q, %v", consumedAt, err)
	}
	if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: agentID, DisplayName: "replay"}); !errors.Is(err, ErrInvalidJoinToken) {
		t.Fatalf("token replay = %v", err)
	}
	if count := agentCount(t, ctx, service); count != 1 {
		t.Fatalf("agent count after replay = %d", count)
	}
}

func TestAgentStateIdentityFlowsUnchangedThroughRegistration(t *testing.T) {
	ctx := context.Background()
	agentStore, _, err := agentstate.Open(ctx, filepath.Join(t.TempDir(), "agent-state"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentStore.Close() })
	snapshot, err := agentStore.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !agentid.Valid(snapshot.AgentID) {
		t.Fatalf("agentstate produced invalid Agent ID %q", snapshot.AgentID)
	}

	service := newTestService(t, ctx)
	issued := mustIssue(t, ctx, service, time.Hour)
	result, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: snapshot.AgentID, DisplayName: "state-backed agent"})
	if err != nil {
		t.Fatal(err)
	}
	var storedAgentID string
	if err := service.store.DB().QueryRowContext(ctx, "SELECT id FROM agents WHERE id = ?", snapshot.AgentID).Scan(&storedAgentID); err != nil {
		t.Fatal(err)
	}
	if result.AgentID != snapshot.AgentID || result.Credential.AgentID != snapshot.AgentID || storedAgentID != snapshot.AgentID {
		t.Fatalf("Agent ID diverged: snapshot=%q result=%q credential=%q db=%q", snapshot.AgentID, result.AgentID, result.Credential.AgentID, storedAgentID)
	}
}

func TestNewRegistrationAgentIDCollisionRollsBackToken(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	agentID := mustAgentID(t)
	first := mustIssue(t, ctx, service, time.Hour)
	if _, err := service.Register(ctx, Request{JoinToken: first.Token, AgentID: agentID, DisplayName: "first"}); err != nil {
		t.Fatal(err)
	}
	second := mustIssue(t, ctx, service, time.Hour)
	if _, err := service.Register(ctx, Request{JoinToken: second.Token, AgentID: agentID, DisplayName: "collision"}); !errors.Is(err, ErrAgentIDCollision) {
		t.Fatalf("Agent ID collision = %v", err)
	}
	var consumed *string
	if err := service.store.DB().QueryRowContext(ctx, "SELECT consumed_at FROM join_tokens WHERE id = ?", second.ID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != nil {
		t.Fatalf("collision consumed token at %q", *consumed)
	}
	if _, err := service.Register(ctx, Request{JoinToken: second.Token, AgentID: mustAgentID(t), DisplayName: "retry"}); err != nil {
		t.Fatalf("collision token was not reusable: %v", err)
	}
}

func TestTokenRaceAllowsExactlyOneRegistration(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	issued := mustIssue(t, ctx, service, time.Hour)
	agentID := mustAgentID(t)
	const contenders = 12
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	var successes atomic.Int32
	errorsSeen := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			_, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: agentID, DisplayName: fmt.Sprintf("agent-%d", i)})
			if err == nil {
				successes.Add(1)
			} else {
				errorsSeen <- err
			}
		}(i)
	}
	start.Done()
	done.Wait()
	close(errorsSeen)
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful registrations = %d, want 1; errors=%v", got, collectErrors(errorsSeen))
	}
	if count := agentCount(t, ctx, service); count != 1 {
		t.Fatalf("agent count = %d, want 1", count)
	}
}

func TestExpiredRevokedWrongAndMalformedTokensAreRejected(t *testing.T) {
	ctx := context.Background()
	t.Run("expired", func(t *testing.T) {
		service := newTestService(t, ctx)
		issued := mustIssue(t, ctx, service, time.Hour)
		service.now = func() time.Time { return fixedNow.Add(time.Hour) }
		if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: mustAgentID(t), DisplayName: "agent"}); !errors.Is(err, ErrInvalidJoinToken) {
			t.Fatalf("expired token = %v", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		service := newTestService(t, ctx)
		issued := mustIssue(t, ctx, service, time.Hour)
		if err := service.RevokeJoinToken(ctx, issued.ID); err != nil {
			t.Fatal(err)
		}
		if err := service.RevokeJoinToken(ctx, issued.ID); err != nil {
			t.Fatalf("idempotent revoke: %v", err)
		}
		if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: mustAgentID(t), DisplayName: "agent"}); !errors.Is(err, ErrInvalidJoinToken) {
			t.Fatalf("revoked token = %v", err)
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		service := newTestService(t, ctx)
		issued := mustIssue(t, ctx, service, time.Hour)
		wrong := issued.ID + "." + base64.RawURLEncoding.EncodeToString(make([]byte, JoinTokenSecretBytes))
		agentID := mustAgentID(t)
		if _, err := service.Register(ctx, Request{JoinToken: wrong, AgentID: agentID, DisplayName: "agent"}); !errors.Is(err, ErrInvalidJoinToken) {
			t.Fatalf("wrong secret = %v", err)
		}
		if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: agentID, DisplayName: "agent"}); err != nil {
			t.Fatalf("wrong secret consumed real token: %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		service := newTestService(t, ctx)
		for _, token := range []string{"", "a", "a.b.c", "join_bad.not-base64", "unknown_00000000000000000000000000000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
			if _, err := service.Register(ctx, Request{JoinToken: token, DisplayName: "agent"}); !errors.Is(err, ErrInvalidJoinToken) {
				t.Fatalf("malformed %q = %v", token, err)
			}
		}
	})
}

func TestRegistrationRollbackLeavesTokenReusable(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	issued := mustIssue(t, ctx, service, time.Hour)
	agentID := mustAgentID(t)
	injected := errors.New("injected after consume")
	service.afterConsume = func() error { return injected }
	if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: agentID, DisplayName: "agent"}); !errors.Is(err, injected) {
		t.Fatalf("registration fault = %v", err)
	}
	var consumed *string
	if err := service.store.DB().QueryRowContext(ctx, "SELECT consumed_at FROM join_tokens WHERE id = ?", issued.ID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != nil || agentCount(t, ctx, service) != 0 {
		t.Fatalf("rollback leaked state: consumed=%v agents=%d", consumed, agentCount(t, ctx, service))
	}
	service.afterConsume = nil
	if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: agentID, DisplayName: "agent"}); err != nil {
		t.Fatalf("rolled-back token not reusable: %v", err)
	}
}

func TestInvalidBoundedFieldsDoNotConsumeToken(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	issued := mustIssue(t, ctx, service, time.Hour)
	tooMany := make(map[string]string)
	for i := 0; i <= MaxMetadataEntries; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	requests := []Request{
		{JoinToken: issued.Token, DisplayName: ""},
		{JoinToken: issued.Token, DisplayName: strings.Repeat("x", MaxDisplayNameBytes+1)},
		{JoinToken: issued.Token, DisplayName: "agent", Metadata: tooMany},
		{JoinToken: issued.Token, DisplayName: "agent", Metadata: map[string]string{strings.Repeat("k", MaxMetadataKeyBytes+1): "v"}},
		{JoinToken: issued.Token, DisplayName: "agent", Metadata: map[string]string{"k": strings.Repeat("v", MaxMetadataValueBytes+1)}},
	}
	for _, request := range requests {
		if _, err := service.Register(ctx, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid bounded request = %v", err)
		}
	}
	if _, err := service.Register(ctx, Request{JoinToken: issued.Token, AgentID: mustAgentID(t), DisplayName: "valid"}); err != nil {
		t.Fatalf("validation failure consumed token: %v", err)
	}
}

func TestExpiredCredentialRejoinPreservesStableAgentID(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	initialToken := mustIssue(t, ctx, service, time.Hour)
	agentID := mustAgentID(t)
	initial, err := service.Register(ctx, Request{JoinToken: initialToken.Token, AgentID: agentID, DisplayName: "old", Metadata: map[string]string{"generation": "one"}})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return fixedNow.Add(identity.CredentialLifetime + time.Hour) }
	rejoinToken, err := service.IssueRejoinToken(ctx, initial.AgentID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Register(ctx, Request{
		JoinToken:   rejoinToken.Token,
		AgentID:     initial.AgentID,
		DisplayName: "restored",
		Metadata:    map[string]string{"generation": "two"},
		Reuse:       &ReuseRequest{ExpiredCredential: initial.Credential},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentID != initial.AgentID || result.Credential.CredentialID == initial.Credential.CredentialID {
		t.Fatalf("identity not preserved with new credential: old=%+v new=%+v", initial, result)
	}
	if count := agentCount(t, ctx, service); count != 1 {
		t.Fatalf("rejoin duplicated Agent row: %d", count)
	}
	var display, metadata string
	if err := service.store.DB().QueryRowContext(ctx, "SELECT display_name, metadata_json FROM agents WHERE id = ?", initial.AgentID).Scan(&display, &metadata); err != nil {
		t.Fatal(err)
	}
	if display != "restored" || !strings.Contains(metadata, `"generation":"two"`) {
		t.Fatalf("rejoin did not refresh bounded fields: %q %q", display, metadata)
	}
}

func TestRejoinRequiresBoundTokenExactExpiredUnrevokedProof(t *testing.T) {
	ctx := context.Background()
	newRegistered := func(t *testing.T) (*Service, Result) {
		service := newTestService(t, ctx)
		result, err := service.Register(ctx, Request{JoinToken: mustIssue(t, ctx, service, time.Hour).Token, AgentID: mustAgentID(t), DisplayName: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		return service, result
	}
	t.Run("general token cannot reuse", func(t *testing.T) {
		service, old := newRegistered(t)
		service.now = func() time.Time { return fixedNow.Add(identity.CredentialLifetime + time.Hour) }
		general := mustIssue(t, ctx, service, time.Hour)
		_, err := service.Register(ctx, Request{JoinToken: general.Token, AgentID: old.AgentID, DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}})
		if !errors.Is(err, ErrIdentityReuseProof) {
			t.Fatalf("general token reuse = %v", err)
		}
	})
	t.Run("bound token cannot create new id", func(t *testing.T) {
		service, old := newRegistered(t)
		bound, err := service.IssueRejoinToken(ctx, old.AgentID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Register(ctx, Request{JoinToken: bound.Token, AgentID: old.AgentID, DisplayName: "agent"}); !errors.Is(err, ErrIdentityReuseProof) {
			t.Fatalf("bound token new registration = %v", err)
		}
	})
	t.Run("active credential", func(t *testing.T) {
		service, old := newRegistered(t)
		bound, err := service.IssueRejoinToken(ctx, old.AgentID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Register(ctx, Request{JoinToken: bound.Token, AgentID: old.AgentID, DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}})
		if !errors.Is(err, ErrIdentityReuseProof) {
			t.Fatalf("active credential rejoin = %v", err)
		}
	})
	t.Run("request id mismatch", func(t *testing.T) {
		service, old := newRegistered(t)
		service.now = func() time.Time { return fixedNow.Add(identity.CredentialLifetime + time.Hour) }
		bound, err := service.IssueRejoinToken(ctx, old.AgentID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Register(ctx, Request{JoinToken: bound.Token, AgentID: mustAgentID(t), DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}})
		if !errors.Is(err, ErrIdentityReuseProof) {
			t.Fatalf("mismatched request Agent ID = %v", err)
		}
	})
	t.Run("revoked credential", func(t *testing.T) {
		service, old := newRegistered(t)
		if err := service.identities.RevokeCredential(old.Credential, fixedNow.Add(time.Hour), "retired credential"); err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return fixedNow.Add(identity.CredentialLifetime + time.Hour) }
		bound, err := service.IssueRejoinToken(ctx, old.AgentID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Register(ctx, Request{JoinToken: bound.Token, AgentID: old.AgentID, DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}})
		if !errors.Is(err, ErrIdentityReuseProof) {
			t.Fatalf("revoked credential rejoin = %v", err)
		}
	})
	t.Run("retired agent", func(t *testing.T) {
		service, old := newRegistered(t)
		if _, err := service.store.DB().ExecContext(ctx, "UPDATE agents SET retired_at = ? WHERE id = ?", databaseTime(fixedNow.Add(time.Hour)), old.AgentID); err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return fixedNow.Add(identity.CredentialLifetime + time.Hour) }
		bound, err := service.IssueRejoinToken(ctx, old.AgentID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Register(ctx, Request{JoinToken: bound.Token, AgentID: old.AgentID, DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}})
		if !errors.Is(err, ErrAgentRetired) {
			t.Fatalf("retired agent rejoin = %v", err)
		}
		// The rejected registry mutation must roll token consumption back.
		if _, err := service.Register(ctx, Request{JoinToken: bound.Token, AgentID: old.AgentID, DisplayName: "agent", Reuse: &ReuseRequest{ExpiredCredential: old.Credential}}); !errors.Is(err, ErrAgentRetired) {
			t.Fatalf("retired rollback retry = %v", err)
		}
	})
}

func TestRecoveryAgentStateAndCredentialLossRequiresNewIdentity(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t, ctx)
	oldAgentID := mustAgentID(t)
	initial, err := service.Register(ctx, Request{
		JoinToken: mustIssue(t, ctx, service, time.Hour).Token,
		AgentID:   oldAgentID, DisplayName: "before state loss",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Possession of a general Join Token and knowledge of the old ID are not
	// identity-reuse proof. The failed collision must not consume the token so
	// the operator can use it for the replacement Agent identity.
	replacementToken := mustIssue(t, ctx, service, time.Hour)
	if _, err := service.Register(ctx, Request{
		JoinToken: replacementToken.Token, AgentID: oldAgentID, DisplayName: "guessed reuse",
	}); !errors.Is(err, ErrAgentIDCollision) {
		t.Fatalf("lost-state reuse of existing Agent ID = %v", err)
	}
	newAgentID := mustAgentID(t)
	replacement, err := service.Register(ctx, Request{
		JoinToken: replacementToken.Token, AgentID: newAgentID, DisplayName: "after state loss",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.AgentID == initial.AgentID || replacement.Credential.AgentID != newAgentID {
		t.Fatalf("replacement identity = %+v, initial = %+v", replacement, initial)
	}
	if count := agentCount(t, ctx, service); count != 2 {
		t.Fatalf("state-loss recovery Agent rows = %d, want old plus replacement", count)
	}

	bound, err := service.IssueRejoinToken(ctx, oldAgentID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(ctx, Request{
		JoinToken: bound.Token, AgentID: oldAgentID, DisplayName: "missing proof",
	}); !errors.Is(err, ErrIdentityReuseProof) {
		t.Fatalf("purpose-bound token without expired credential proof = %v", err)
	}
}

func newTestService(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	dir := t.TempDir()
	store, err := serverstore.Open(ctx, filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identities, err := identity.Open(filepath.Join(dir, "identity", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(store, identities)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return fixedNow }
	return service
}

func mustIssue(t *testing.T, ctx context.Context, service *Service, lifetime time.Duration) IssuedToken {
	t.Helper()
	issued, err := service.IssueJoinToken(ctx, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func mustAgentID(t *testing.T) string {
	t.Helper()
	id, err := agentid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func agentCount(t *testing.T, ctx context.Context, service *Service) int {
	t.Helper()
	var count int
	if err := service.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM agents").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func joinTokenColumns(t *testing.T, ctx context.Context, service *Service) []string {
	t.Helper()
	rows, err := service.store.DB().QueryContext(ctx, "PRAGMA table_info(join_tokens)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func collectErrors(errorsSeen <-chan error) []string {
	var out []string
	for err := range errorsSeen {
		out = append(out, err.Error())
	}
	return out
}
