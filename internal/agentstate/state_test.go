package agentstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
)

func TestOpenCreatesStableIdentityAndAdvancesIncarnation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, startup, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Ready() {
		t.Fatal("newly opened Store is not ready")
	}
	if startup.PreviousIncarnation != 0 || startup.CurrentIncarnation != 1 || startup.PreviousUnclean {
		t.Fatalf("first startup = %+v", startup)
	}
	if !agentid.Valid(startup.AgentID) {
		t.Fatalf("agent_id is not canonical UUIDv4: %q", startup.AgentID)
	}

	stateInfo, err := os.Lstat(filepath.Join(dir, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", stateInfo.Mode().Perm())
	}

	closedAt := time.Date(2026, 8, 15, 1, 2, 3, 4, time.UTC)
	if err := store.GracefulClose(ctx, 3, closedAt); err != nil {
		t.Fatal(err)
	}
	if store.Ready() {
		t.Fatal("gracefully closed Store is still ready")
	}

	tail := Cursor{Incarnation: 1, Seq: 3}
	reopened, second, err := Open(ctx, dir, &tail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if second.AgentID != startup.AgentID {
		t.Fatalf("agent_id changed across reopen: %q -> %q", startup.AgentID, second.AgentID)
	}
	if second.PreviousIncarnation != 1 || second.CurrentIncarnation != 2 || second.PreviousUnclean {
		t.Fatalf("second startup = %+v", second)
	}
	if second.KnownDurableThrough == nil || *second.KnownDurableThrough != tail {
		t.Fatalf("known durable tail = %+v, want %+v", second.KnownDurableThrough, tail)
	}
}

func TestDockerEventWatermarkIsDurableAndMonotonic(t *testing.T) {
	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 15, 1, 2, 3, 4, time.UTC)
	digest := strings.Repeat("a", 64)
	if err := store.AdvanceLastDockerEventAt(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDockerSnapshotSHA256(ctx, digest); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDockerSnapshotSHA256(ctx, "not-a-digest"); err == nil {
		t.Fatal("invalid Docker snapshot digest was accepted")
	}
	if err := store.AdvanceLastDockerEventAt(ctx, at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil || !snapshot.LastDockerEventAt.Equal(at) || snapshot.DockerSnapshotSHA256 != digest {
		t.Fatalf("watermark = %s, %v", snapshot.LastDockerEventAt, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err = reopened.Snapshot()
	if err != nil || !snapshot.LastDockerEventAt.Equal(at) || snapshot.DockerSnapshotSHA256 != digest {
		t.Fatalf("reopened watermark = %s, %v", snapshot.LastDockerEventAt, err)
	}
}

func TestUncleanAndWALMismatchAssessment(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, startup, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !startup.PreviousUnclean {
		t.Fatal("missing clean_close was not reported as unclean")
	}
	if err := reopened.GracefulClose(ctx, 5, time.Now()); err != nil {
		t.Fatal(err)
	}

	wrongTail := Cursor{Incarnation: 2, Seq: 4}
	again, startup, err := Open(ctx, dir, &wrongTail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if !startup.PreviousUnclean {
		t.Fatal("clean_close/WAL tail mismatch was not reported as unclean")
	}
}

func TestWALAheadOfStateIsTypedInvariantError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = Open(ctx, dir, &Cursor{Incarnation: 2, Seq: 1})
	if !errors.Is(err, ErrStateInvariant) {
		t.Fatalf("Open error = %v, want ErrStateInvariant", err)
	}
}

func TestOpenRejectsNonCanonicalOrNonV4AgentID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, replacement := range map[string]string{
		"legacy format": "agt_0123456789abcdef0123456789abcdef",
		"uppercase":     "550E8400-E29B-41D4-A716-446655440000",
		"version one":   "550e8400-e29b-11d4-a716-446655440000",
	} {
		t.Run(name, func(t *testing.T) {
			dir := newStateDir(t)
			store, startup, err := Open(ctx, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, StateFileName)
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			payload = []byte(strings.Replace(string(payload), startup.AgentID, replacement, 1))
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(ctx, dir, nil); !errors.Is(err, ErrStateInvariant) {
				t.Fatalf("Open invalid Agent ID = %v", err)
			}
		})
	}
}

func TestCredentialAndArchiveRebindPersistAcrossReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential := Credential{Data: []byte("signed-agent-credential")}
	if err := store.SetCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}

	coverage1 := Cursor{Incarnation: 1, Seq: 1}
	first, err := store.BindArchive(ctx, "server-1", 1, "archive-1", coverage1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Previous != nil {
		t.Fatalf("initial binding result = %+v", first)
	}
	if err := store.AdvanceArchiveACK(ctx, "archive-1", Cursor{Incarnation: 1, Seq: 9}); err != nil {
		t.Fatal(err)
	}

	idempotent, err := store.BindArchive(ctx, "server-1", 1, "archive-1", Cursor{Incarnation: 9, Seq: 9}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.Changed || idempotent.Current.CoverageBeginsAt != coverage1 {
		t.Fatalf("idempotent binding result = %+v", idempotent)
	}

	coverage2 := Cursor{Incarnation: 1, Seq: 10}
	reboundAt := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	rebound, err := store.BindArchive(ctx, "server-1", 2, "archive-2", coverage2, reboundAt)
	if err != nil {
		t.Fatal(err)
	}
	if !rebound.Changed || rebound.Previous == nil {
		t.Fatalf("forward rebind result = %+v", rebound)
	}
	if rebound.Previous.AckedThrough == nil || *rebound.Previous.AckedThrough != (Cursor{Incarnation: 1, Seq: 9}) {
		t.Fatalf("retired ACK = %+v", rebound.Previous.AckedThrough)
	}
	if rebound.Current.AckedThrough != nil {
		t.Fatalf("new archive inherited ACK: %+v", rebound.Current.AckedThrough)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Credential.Data) != string(credential.Data) {
		t.Fatalf("credential data = %q", snapshot.Credential.Data)
	}
	if snapshot.BoundArchive == nil || snapshot.BoundArchive.Generation != 2 ||
		snapshot.BoundArchive.ArchiveID != "archive-2" || snapshot.BoundArchive.AckedThrough != nil {
		t.Fatalf("reopened binding = %+v", snapshot.BoundArchive)
	}
	if len(snapshot.RetiredArchives) != 1 || snapshot.RetiredArchives[0].ArchiveID != "archive-1" {
		t.Fatalf("retired archives = %+v", snapshot.RetiredArchives)
	}

	stateData, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	stateText := strings.ToLower(string(stateData))
	if strings.Contains(stateText, "wal_bytes") || strings.Contains(stateText, "backup_bytes") {
		t.Fatalf("state contains excluded payload field: %s", stateData)
	}
}

func TestArchiveRollbackAndInvariantErrorsAreTyped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, err := Open(ctx, newStateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	coverage := Cursor{Incarnation: 1, Seq: 1}
	if _, err := store.BindArchive(ctx, "server-1", 4, "archive-4", coverage, time.Now()); err != nil {
		t.Fatal(err)
	}

	_, err = store.BindArchive(ctx, "server-1", 3, "archive-3", coverage, time.Now())
	if !errors.Is(err, ErrArchiveRollbackDetected) {
		t.Fatalf("lower generation error = %v", err)
	}
	var rollback *ArchiveRollbackError
	if !errors.As(err, &rollback) || rollback.BoundGeneration != 4 || rollback.PresentedGeneration != 3 {
		t.Fatalf("rollback details = %+v", rollback)
	}

	_, err = store.BindArchive(ctx, "server-1", 4, "different", coverage, time.Now())
	if !errors.Is(err, ErrArchiveInvariant) {
		t.Fatalf("same-generation identity error = %v", err)
	}
	_, err = store.BindArchive(ctx, "server-1", 5, "archive-4", coverage, time.Now())
	if !errors.Is(err, ErrArchiveInvariant) {
		t.Fatalf("archive id reuse error = %v", err)
	}
	_, err = store.BindArchive(ctx, "server-2", 5, "archive-5", coverage, time.Now())
	if !errors.Is(err, ErrServerIdentityMismatch) {
		t.Fatalf("server identity mismatch error = %v", err)
	}
}

func TestACKCannotRegress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, err := Open(ctx, newStateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.BindArchive(ctx, "server", 1, "archive", Cursor{1, 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceArchiveACK(ctx, "archive", Cursor{2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceArchiveACK(ctx, "archive", Cursor{2, 3}); err != nil {
		t.Fatalf("idempotent ACK: %v", err)
	}
	if err := store.AdvanceArchiveACK(ctx, "archive", Cursor{1, 99}); !errors.Is(err, ErrCursorRollback) {
		t.Fatalf("regressing ACK error = %v", err)
	}
	if err := store.AdvanceArchiveACK(ctx, "retired-archive", Cursor{3, 1}); !errors.Is(err, ErrArchiveInvariant) {
		t.Fatalf("wrong archive ACK error = %v", err)
	}
}

func TestConcurrentRebindIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, err := Open(ctx, newStateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.BindArchive(ctx, "server", 1, "archive-1", Cursor{1, 1}, time.Now()); err != nil {
		t.Fatal(err)
	}

	const callers = 16
	results := make(chan RebindResult, callers)
	errorsFound := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := store.BindArchive(
				ctx, "server", 2, "archive-2", Cursor{1, 2}, time.Now(),
			)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent BindArchive: %v", err)
	}
	changed := 0
	for result := range results {
		if result.Changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed results = %d, want 1", changed)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.RetiredArchives) != 1 {
		t.Fatalf("retired archive count = %d, want 1", len(snapshot.RetiredArchives))
	}
}

func TestSecondProcessLockIsRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	first, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	_, _, err = Open(ctx, dir, nil)
	if !errors.Is(err, ErrStateLocked) {
		t.Fatalf("second Open error = %v, want ErrStateLocked", err)
	}
}

func TestSymlinkAndInsecureModesAreRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	t.Run("directory symlink", func(t *testing.T) {
		base := t.TempDir()
		realDir := filepath.Join(base, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		_, _, err := Open(ctx, link, nil)
		if !errors.Is(err, ErrSymlink) {
			t.Fatalf("directory symlink error = %v", err)
		}
	})

	t.Run("state symlink", func(t *testing.T) {
		dir := newStateDir(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, StateFileName)); err != nil {
			t.Fatal(err)
		}
		_, _, err := Open(ctx, dir, nil)
		if !errors.Is(err, ErrSymlink) {
			t.Fatalf("state symlink error = %v", err)
		}
	})

	t.Run("insecure directory", func(t *testing.T) {
		dir := newStateDir(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := Open(ctx, dir, nil)
		if !errors.Is(err, ErrInsecureMode) {
			t.Fatalf("insecure directory error = %v", err)
		}
	})

	t.Run("insecure state file", func(t *testing.T) {
		dir := newStateDir(t)
		store, _, err := Open(ctx, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, StateFileName), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err = Open(ctx, dir, nil)
		if !errors.Is(err, ErrInsecureMode) {
			t.Fatalf("insecure state error = %v", err)
		}
	})
}

func TestPersistenceFaultPoisonsStoreWithoutPublishingTransition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fault := errors.New("fault")
	store.hooks.beforeRename = func() error { return fault }
	if err := store.SetCredential(ctx, Credential{Data: []byte("new")}); !errors.Is(err, fault) {
		t.Fatalf("SetCredential error = %v", err)
	}
	if store.Ready() {
		t.Fatal("Store remained ready after persistence fault")
	}
	if err := store.SetCredential(ctx, Credential{Data: []byte("retry")}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("retry error = %v, want ErrNotReady", err)
	}

	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Credential.Data) != 0 {
		t.Fatalf("failed transition published in memory: %q", snapshot.Credential.Data)
	}
}

func TestDirectorySyncFaultRequiresReopenAndRecoversCommittedFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := newStateDir(t)
	store, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("directory sync fault")
	store.hooks.beforeDirSync = func() error { return fault }
	if err := store.SetCredential(ctx, Credential{FileReference: "credential.json"}); !errors.Is(err, fault) {
		t.Fatalf("SetCredential error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Credential.FileReference != "credential.json" {
		t.Fatalf("recovered credential reference = %q", snapshot.Credential.FileReference)
	}
}

func TestInitialFileSyncFaultDoesNotReturnReadyStore(t *testing.T) {
	t.Parallel()

	fault := errors.New("file sync fault")
	store, _, err := openWithHooks(
		context.Background(), newStateDir(t), nil,
		persistHooks{beforeFileSync: func() error { return fault }},
	)
	if !errors.Is(err, fault) {
		t.Fatalf("Open error = %v", err)
	}
	if store != nil {
		t.Fatal("Open returned a Store before durable startup completed")
	}
}

func TestCredentialFormsAreExclusiveAndSnapshotsAreDefensive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, err := Open(ctx, newStateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	credential := Credential{FileReference: "credential.json", Data: []byte("also-inline")}
	if err := store.SetCredential(ctx, credential); !errors.Is(err, ErrStateInvariant) {
		t.Fatalf("mixed credential error = %v", err)
	}

	data := []byte("secret")
	if err := store.SetCredential(ctx, Credential{Data: data}); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Credential.Data) != "secret" {
		t.Fatalf("caller mutated stored credential: %q", snapshot.Credential.Data)
	}
	snapshot.Credential.Data[0] = 'Y'
	again, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Credential.Data) != "secret" {
		t.Fatalf("snapshot mutated stored credential: %q", again.Credential.Data)
	}
}

func TestInspectIsReadOnlyAndDoesNotCreateMissingState(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	inspection, err := Inspect(missing)
	if err != nil || inspection.Exists {
		t.Fatalf("missing inspection = %+v, %v", inspection, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("Inspect created missing directory: %v", err)
	}

	dir := newStateDir(t)
	store, startup, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, StateFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Exists || inspection.AgentID != startup.AgentID || inspection.CurrentIncarnation != 1 {
		t.Fatalf("inspection = %+v", inspection)
	}
	if string(before) != string(after) {
		t.Fatal("Inspect rewrote Agent state")
	}
}

func TestCredentialAndArchiveInstallAndRenewalJournalAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, err := Open(ctx, newStateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	coverage := Cursor{Incarnation: 1, Seq: 1}
	if _, err := store.InstallCredentialAndBind(ctx, Credential{Data: []byte("first")},
		"server", 1, "archive-1", coverage, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Credential.Data) != "first" || snapshot.BoundArchive == nil || snapshot.BoundArchive.ArchiveID != "archive-1" {
		t.Fatalf("installed state = %+v", snapshot)
	}
	if _, err := store.StageCredentialRenewalAndBind(ctx,
		Credential{Data: []byte("first")}, Credential{Data: []byte("second")}, "credential-2",
		"server", 2, "archive-2", coverage, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Credential.Data) != "second" || snapshot.PendingActivation == nil ||
		string(snapshot.PendingActivation.Previous.Data) != "first" || snapshot.BoundArchive.ArchiveID != "archive-2" {
		t.Fatalf("staged renewal = %+v", snapshot)
	}
	if err := store.CompleteCredentialActivation(ctx, "wrong"); !errors.Is(err, ErrStateInvariant) {
		t.Fatalf("wrong activation ID error = %v", err)
	}
	if err := store.CompleteCredentialActivation(ctx, "credential-2"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PendingActivation != nil {
		t.Fatalf("pending activation = %+v", snapshot.PendingActivation)
	}
}

func newStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
