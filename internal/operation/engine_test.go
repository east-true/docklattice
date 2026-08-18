package operation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func TestAcceptedCancelPropagatesToDetachedRunner(t *testing.T) {
	engine := NewDefault()
	runnerStarted := make(chan struct{})
	runnerCanceled := make(chan error, 1)
	record, created, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "cancel-propagation", ProjectKey: "project-a", Type: TypeComposePull,
	}, func(ctx context.Context, operation *Operation) {
		if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
			runnerCanceled <- err
			return
		}
		close(runnerStarted)
		<-ctx.Done()
		runnerCanceled <- context.Cause(ctx)
		_ = operation.TransitionStatus(StatusCanceled, "", "")
	})
	if err != nil || !created || record.Status != StatusRequested {
		t.Fatalf("StartOperation = %#v, %v, %v", record, created, err)
	}
	<-runnerStarted
	if outcome, err := engine.CancelWithError(record.OperationID, CancelReasonUser); err != nil || outcome != CancelAccepted {
		t.Fatalf("Cancel = %s, %v", outcome, err)
	}
	if cause := <-runnerCanceled; cause == nil || !strings.Contains(cause.Error(), "USER") {
		t.Fatalf("runner cancellation cause = %v", cause)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, _ := engine.Get(record.OperationID)
		if got.Status == StatusCanceled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not become canceled: %#v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func testEngine(t *testing.T, edit func(*Config)) *Engine {
	t.Helper()
	config := DefaultConfig()
	config.Clock = &fakeClock{now: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)}
	config.ProjectLockWait = 10 * time.Millisecond
	if edit != nil {
		edit(&config)
	}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func create(t *testing.T, engine *Engine, id string, operationType Type, project string) *Operation {
	t.Helper()
	operation, created, err := engine.Create(context.Background(), Spec{OperationID: id, Type: operationType, ProjectKey: project, Target: "target"})
	if err != nil || !created {
		t.Fatalf("Create() operation=%v created=%v err=%v", operation, created, err)
	}
	return operation
}

func start(t *testing.T, operation *Operation) {
	t.Helper()
	if err := operation.TransitionStatus(StatusDispatched, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(PhaseExecuting); err != nil {
		t.Fatal(err)
	}
}

func fail(t *testing.T, operation *Operation) {
	t.Helper()
	record := operation.Snapshot()
	if record.Status == StatusRequested {
		if err := operation.TransitionStatus(StatusDispatched, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := operation.TransitionStatus(StatusFailed, "", "failed"); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyAndSpecMismatch(t *testing.T) {
	engine := testEngine(t, nil)
	spec := Spec{OperationID: "same", ProjectKey: "project", Target: "service-a", Type: TypeComposeUp}
	first, created, err := engine.Create(context.Background(), spec)
	if err != nil || !created {
		t.Fatalf("first Create() created=%v err=%v", created, err)
	}
	second, created, err := engine.Create(context.Background(), spec)
	if err != nil || created || first != second {
		t.Fatalf("retry Create() same=%v created=%v err=%v", first == second, created, err)
	}
	spec.Target = "service-b"
	if _, _, err := engine.Create(context.Background(), spec); !HasErrorCode(err, CodeSpecMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestIdempotencyIncludesPayloadHashWithoutPersistingPayload(t *testing.T) {
	engine := NewDefault()
	spec := Spec{OperationID: "payload", ProjectKey: "project", Type: TypeComposeUp, PayloadHash: strings.Repeat("a", 64)}
	operation, created, err := engine.Create(context.Background(), spec)
	if err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}
	if got := operation.Snapshot().PayloadHash; got != spec.PayloadHash {
		t.Fatalf("payload hash = %q", got)
	}
	spec.PayloadHash = strings.Repeat("b", 64)
	if _, _, err := engine.Create(context.Background(), spec); !HasErrorCode(err, CodeSpecMismatch) {
		t.Fatalf("changed payload hash error = %v", err)
	}
}

func TestCreateRejectsUnknownOperationType(t *testing.T) {
	engine := NewDefault()
	_, _, err := engine.Create(context.Background(), Spec{
		OperationID: "unknown", ProjectKey: "project", Type: Type("shell.exec"),
	})
	if !HasErrorCode(err, CodeInvalidSpec) {
		t.Fatalf("Create unknown type error = %v", err)
	}
}

func TestProjectLockBusyAndRelease(t *testing.T) {
	engine := testEngine(t, func(config *Config) { config.ProjectLockWait = 5 * time.Millisecond })
	first := create(t, engine, "first", TypeComposeUp, "project")
	second, created, err := engine.Create(context.Background(), Spec{OperationID: "second", Type: TypeContainerRestart, ProjectKey: "project"})
	if !created || !HasErrorCode(err, CodeProjectBusy) {
		t.Fatalf("busy Create() created=%v err=%v", created, err)
	}
	if record := second.Snapshot(); record.Status != StatusRejected || record.Error == "" {
		t.Fatalf("busy record = %#v", record)
	}
	fail(t, first)
	if _, created, err := engine.Create(context.Background(), Spec{OperationID: "third", Type: TypeComposeDown, ProjectKey: "project"}); err != nil || !created {
		t.Fatalf("Create() after release created=%v err=%v", created, err)
	}
}

func TestProjectLockWaitsForOwner(t *testing.T) {
	engine := testEngine(t, func(config *Config) { config.ProjectLockWait = time.Second })
	first := create(t, engine, "first", TypeComposeUp, "project")
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.Create(context.Background(), Spec{OperationID: "second", Type: TypeComposeDown, ProjectKey: "project"})
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	fail(t, first)
	if err := <-done; err != nil {
		t.Fatalf("waiter failed after release: %v", err)
	}
}

func TestAcceptedCancelKeepsLockUntilCanceledTerminal(t *testing.T) {
	engine := testEngine(t, func(config *Config) { config.ProjectLockWait = 0 })
	first := create(t, engine, "first", TypeComposeUp, "project")
	start(t, first)
	if got := engine.Cancel("first", CancelReasonUser); got != CancelAccepted {
		t.Fatalf("Cancel() = %s", got)
	}
	if _, _, err := engine.Create(context.Background(), Spec{OperationID: "second", Type: TypeComposeDown, ProjectKey: "project"}); !HasErrorCode(err, CodeProjectBusy) {
		t.Fatalf("lock was released before process cleanup: %v", err)
	}
	if err := first.TransitionStatus(StatusCanceled, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, created, err := engine.Create(context.Background(), Spec{OperationID: "third", Type: TypeComposeDown, ProjectKey: "project"}); err != nil || !created {
		t.Fatalf("Create() after canceled terminal created=%v err=%v", created, err)
	}
}

func TestResultRingBoundsAndNeverEvictsActive(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)}
	config := DefaultConfig()
	config.Clock = clock
	config.ProjectLockWait = 0
	config.ResultMax = 2
	config.ResultRetention = time.Hour
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	create(t, engine, "active", TypeDiscoveryRescan, "")
	for _, id := range []string{"one", "two", "three"} {
		op := create(t, engine, id, TypeDiscoveryRescan, "")
		fail(t, op)
		clock.Advance(time.Minute)
	}
	if _, ok := engine.Get("one"); ok {
		t.Fatal("oldest result was not evicted by count")
	}
	if _, ok := engine.Get("active"); !ok {
		t.Fatal("active operation was evicted")
	}
	clock.Advance(2 * time.Hour)
	if _, ok := engine.Get("two"); ok {
		t.Fatal("expired result was not evicted")
	}
	if _, ok := engine.Get("three"); ok {
		t.Fatal("expired result was not evicted")
	}
	if _, ok := engine.Get("active"); !ok {
		t.Fatal("active operation was evicted by retention")
	}
}

func TestDisconnectDoesNotCancel(t *testing.T) {
	engine := testEngine(t, nil)
	operation := create(t, engine, "op", TypeComposeUp, "project")
	start(t, operation)
	before := operation.Snapshot()
	if !engine.HandleDisconnect("op", DisconnectBrowser) || !engine.HandleDisconnect("op", DisconnectTransport) {
		t.Fatal("known operation disconnect was not acknowledged")
	}
	after := operation.Snapshot()
	if after.Status != before.Status || after.Phase != before.Phase || after.Revision != before.Revision {
		t.Fatalf("disconnect mutated operation: before=%#v after=%#v", before, after)
	}
}

func TestCreateHonorsCanceledLockWait(t *testing.T) {
	engine := testEngine(t, func(config *Config) { config.ProjectLockWait = time.Second })
	create(t, engine, "first", TypeComposeUp, "project")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	operation, created, err := engine.Create(ctx, Spec{OperationID: "second", Type: TypeComposeDown, ProjectKey: "project"})
	if !created || !errors.Is(err, context.Canceled) || operation.Snapshot().Status != StatusRejected {
		t.Fatalf("Create() operation=%#v created=%v err=%v", operation.Snapshot(), created, err)
	}
}
