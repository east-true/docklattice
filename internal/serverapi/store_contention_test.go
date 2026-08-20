package serverapi

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/serverstore"
	"github.com/east-true/dockpilot/internal/webui"
)

// TestStoreContentionIsTransientNotInternal is the contract behind the HTTP 500
// the abuse matrix caught: SQLite write contention is load, and the API answers
// it as such. Anything else keeps the meaning it already had.
func TestStoreContentionIsTransientNotInternal(t *testing.T) {
	t.Parallel()

	if got := classifyStoreBusy(nil); got != nil {
		t.Fatalf("classifyStoreBusy(nil) = %v, want nil", got)
	}

	unrelated := errors.New("serverapi: something else went wrong")
	if got := classifyStoreBusy(unrelated); !errors.Is(got, unrelated) {
		t.Fatalf("classifyStoreBusy kept %v as %v, want it unchanged", unrelated, got)
	}
	if errors.Is(classifyStoreBusy(unrelated), webui.ErrBusy) {
		t.Fatal("an unrelated failure was reported as database contention")
	}

	busy := fmt.Errorf("serverapi: update recovered operation: %w", serverstore.ErrBusy)
	classified := classifyStoreBusy(busy)
	if !errors.Is(classified, webui.ErrBusy) {
		t.Fatalf("classifyStoreBusy(busy) = %v, want a busy answer", classified)
	}
	// webui maps ErrBusy to 503 SERVER_BUSY; what matters here is that it is
	// not left unmapped, because unmapped is 500 "Server invariant failure".
	for _, mapped := range []error{webui.ErrNotFound, webui.ErrConflict, webui.ErrInvalidRequest, webui.ErrTooLarge} {
		if errors.Is(classified, mapped) {
			t.Fatalf("database contention was classified as %v", mapped)
		}
	}
}

// TestConcurrentRecoveryMergesDoNotContend drives the shape the operation flood
// produced: repeated recovery merges racing the backup index sync, which is the
// pair that used to leave one side holding a stale read snapshot.
func TestConcurrentRecoveryMergesDoNotContend(t *testing.T) {
	t.Parallel()

	ctx, backend, store, _ := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "agent-a", `{}`)

	const rounds = 24
	errs := make(chan error, rounds*2)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func(round int) {
			defer wg.Done()
			errs <- backend.mergeRecoveredOperations(ctx, nil)
		}(i)
		wg.Add(1)
		go func(round int) {
			defer wg.Done()
			errs <- backend.syncBackupIndex(ctx, "agent-a", "", []webui.Backup{{
				ID:        fmt.Sprintf("backup-%d", round),
				CreatedAt: time.Now().UTC(),
				Trigger:   "manual",
			}})
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, webui.ErrBusy) || serverstore.Busy(err) {
			t.Fatalf("concurrent write transactions contended: %v", err)
		}
		// Other errors are the paths' own validation and are not this test's
		// subject; contention is.
		t.Logf("non-contention error (ignored by this test): %v", err)
	}
}
