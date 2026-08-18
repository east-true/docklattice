package operation

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func shutdownEngine(t *testing.T, auditor TerminalAuditor) *Engine {
	t.Helper()
	config := DefaultConfig()
	config.ProjectLockWait = 0
	if auditor != nil {
		state := t.TempDir()
		if err := os.Chmod(state, 0o700); err != nil {
			t.Fatal(err)
		}
		journal, err := NewFileJournal(state, nil)
		if err != nil {
			t.Fatal(err)
		}
		config.Journal = journal
		config.TerminalAuditor = auditor
	}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestShutdownCancelsThroughAgentShutdownPathAndWaitsForCleanupAndAudit(t *testing.T) {
	auditor := &recordingTerminalAuditor{deliveries: make(map[string]int), confirms: make(map[string]int)}
	engine := shutdownEngine(t, auditor)
	started := make(chan struct{})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	runnerDone := make(chan struct{})
	_, created, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "shutdown-safe", Type: TypeComposePull, ProjectKey: "project",
	}, func(ctx context.Context, current *Operation) {
		defer close(runnerDone)
		if err := current.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Errorf("running: %v", err)
			return
		}
		if err := current.AdvancePhase(PhaseExecuting); err != nil {
			t.Errorf("executing: %v", err)
			return
		}
		close(started)
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		if err := current.TransitionStatus(StatusCanceled, "", context.Cause(ctx).Error()); err != nil {
			t.Errorf("canceled: %v", err)
		}
	})
	if err != nil || !created {
		t.Fatalf("StartOperation created=%v err=%v", created, err)
	}
	<-started

	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { shutdownDone <- engine.Shutdown(ctx) }()
	<-cleanupStarted
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before runner cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCleanup)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	<-runnerDone
	record, ok := engine.Get("shutdown-safe")
	if !ok || record.Status != StatusCanceled || record.CancelReason != CancelReasonAgentShutdown ||
		record.CancelRequestedAt.IsZero() || record.ManagedAuditDelivery != ManagedAuditDelivered {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
	auditor.mu.Lock()
	deliveries, confirms := auditor.deliveries[record.OperationID], auditor.confirms[record.OperationID]
	auditor.mu.Unlock()
	if deliveries != 1 || confirms < 1 {
		t.Fatalf("deliveries=%d confirms=%d", deliveries, confirms)
	}
}

func TestShutdownHonorsTooLateCommitAndContextBound(t *testing.T) {
	engine := shutdownEngine(t, nil)
	committed := make(chan struct{})
	release := make(chan struct{})
	_, _, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "shutdown-commit", Type: TypeBackupRestore, ProjectKey: "project",
	}, func(_ context.Context, current *Operation) {
		if err := current.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Errorf("running: %v", err)
			return
		}
		if err := current.AdvancePhase(PhaseExecuting); err != nil {
			t.Errorf("executing: %v", err)
			return
		}
		if err := current.EnterCommit(); err != nil {
			t.Errorf("commit: %v", err)
			return
		}
		close(committed)
		<-release
		if err := current.AdvancePhase(PhaseFinalizing); err != nil {
			t.Errorf("finalizing: %v", err)
			return
		}
		if err := current.TransitionStatus(StatusSuccess, "done", ""); err != nil {
			t.Errorf("success: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-committed
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if err := engine.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v", err)
	}
	before, _ := engine.Get("shutdown-commit")
	if before.Status != StatusRunning || !before.CancelRequestedAt.IsZero() || before.Phase != PhaseCommitting {
		t.Fatalf("TOO_LATE operation was mutated: %#v", before)
	}
	close(release)
	long, cancelLong := context.WithTimeout(context.Background(), time.Second)
	defer cancelLong()
	if err := engine.Shutdown(long); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Get("shutdown-commit")
	if after.Status != StatusSuccess {
		t.Fatalf("record=%#v", after)
	}
}

func TestShutdownRejectsNewOperationsAndDoesNotUseDisconnect(t *testing.T) {
	engine := shutdownEngine(t, nil)
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "after-shutdown", Type: TypeDiscoveryRescan,
	}, func(context.Context, *Operation) {}); !HasErrorCode(err, CodeAgentShuttingDown) {
		t.Fatalf("StartOperation error=%v", err)
	}
	if engine.HandleDisconnect("after-shutdown", DisconnectTransport) {
		t.Fatal("rejected operation unexpectedly exists")
	}
}

func TestShutdownWaitsForTerminalEvenWhenRunnerReturnsEarly(t *testing.T) {
	engine := shutdownEngine(t, nil)
	started := make(chan struct{})
	_, _, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "bad-runner", Type: TypeComposePull, ProjectKey: "project",
	}, func(ctx context.Context, current *Operation) {
		if err := current.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Errorf("running: %v", err)
			return
		}
		close(started)
		<-ctx.Done()
		// A buggy runner that skips terminal persistence must not let Shutdown
		// report success merely because its goroutine returned.
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := engine.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v", err)
	}
	record, _ := engine.Get("bad-runner")
	if record.Status.Terminal() {
		t.Fatalf("Shutdown fabricated terminal state: %#v", record)
	}
}

func TestShutdownRetriesPendingManagedAuditWithinBound(t *testing.T) {
	auditor := &recordingTerminalAuditor{
		deliveries: make(map[string]int), confirms: make(map[string]int), failDelivery: errors.New("WAL temporarily unavailable"),
	}
	engine := shutdownEngine(t, auditor)
	runnerDone := make(chan struct{})
	_, _, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "audit-retry", Type: TypeDiscoveryRescan,
	}, func(_ context.Context, current *Operation) {
		defer close(runnerDone)
		_ = current.TransitionStatus(StatusRunning, "", "")
		_ = current.AdvancePhase(PhaseExecuting)
		_ = current.AdvancePhase(PhaseFinalizing)
		_ = current.TransitionStatus(StatusSuccess, "done", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runnerDone
	if pending := engine.PendingTerminalAudits(); len(pending) != 1 {
		t.Fatalf("pending=%#v", pending)
	}

	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { done <- engine.Shutdown(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned before audit recovered: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	auditor.mu.Lock()
	auditor.failDelivery = nil
	auditor.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pending := engine.PendingTerminalAudits(); len(pending) != 0 {
		t.Fatalf("pending after retry=%#v", pending)
	}
}
