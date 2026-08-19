package operation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two racing requests carrying the same operation ID and spec must produce one
// execution and one shared record.
func TestConcurrentDuplicateStartExecutesExactlyOnce(t *testing.T) {
	engine := NewDefault()
	var executions atomic.Int64
	proceed := make(chan struct{})
	spec := Spec{OperationID: "duplicate-race", ProjectKey: "project-a", Type: TypeComposePull}
	runner := func(_ context.Context, operation *Operation) {
		executions.Add(1)
		<-proceed
		_ = operation.TransitionStatus(StatusRunning, "", "")
		_ = operation.AdvancePhase(PhaseExecuting)
		_ = operation.AdvancePhase(PhaseFinalizing)
		_ = operation.TransitionStatus(StatusSuccess, "done", "")
	}

	const callers = 16
	created := make([]bool, callers)
	errs := make([]error, callers)
	var start, group sync.WaitGroup
	start.Add(1)
	for index := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			start.Wait()
			_, wasCreated, err := engine.StartOperation(context.Background(), spec, runner)
			created[index], errs[index] = wasCreated, err
		}()
	}
	start.Done()
	group.Wait()

	winners := 0
	for index := range callers {
		if errs[index] != nil {
			t.Fatalf("caller %d: %v", index, errs[index])
		}
		if created[index] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("callers that created the operation = %d", winners)
	}
	close(proceed)
	waitForTerminal(t, engine, spec.OperationID)
	if got := executions.Load(); got != 1 {
		t.Fatalf("runner executions = %d", got)
	}
	record, _ := engine.Get(spec.OperationID)
	if record.Status != StatusSuccess || record.Result != "done" {
		t.Fatalf("record = %+v", record)
	}
	// A retransmission after the operation finished returns the same result and
	// never runs it again.
	replay, wasCreated, err := engine.StartOperation(context.Background(), spec, runner)
	if err != nil || wasCreated || replay.Status != StatusSuccess {
		t.Fatalf("replay = %+v created=%v err=%v", replay, wasCreated, err)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("replay re-executed the operation: %d", got)
	}
}

// A user cancel and a timeout cancel landing together must produce one terminal
// result, one recorded reason, and one process cancellation.
func TestUserAndTimeoutCancelCollapseToOneTerminalResult(t *testing.T) {
	engine := NewDefault()
	var kills atomic.Int64
	started := make(chan struct{})
	finished := make(chan struct{})
	record, _, err := engine.StartOperation(context.Background(), Spec{
		OperationID: "cancel-race", ProjectKey: "project-b", Type: TypeComposePull,
	}, func(ctx context.Context, operation *Operation) {
		_ = operation.TransitionStatus(StatusRunning, "", "")
		close(started)
		<-ctx.Done()
		kills.Add(1)
		_ = operation.TransitionStatus(StatusCanceled, "", "")
		close(finished)
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	outcomes := make([]CancelOutcome, 2)
	var release, group sync.WaitGroup
	release.Add(1)
	for index, reason := range []CancelReason{CancelReasonUser, CancelReasonTimeout} {
		group.Add(1)
		go func() {
			defer group.Done()
			release.Wait()
			outcomes[index] = engine.Cancel(record.OperationID, reason)
		}()
	}
	release.Done()
	group.Wait()
	accepted := 0
	for index, outcome := range outcomes {
		switch outcome {
		case CancelAccepted:
			accepted++
		case CancelAlreadyTerminal:
			// The runner can win the race and finish before the second caller
			// is scheduled; that is a terminal answer, not a second cancel.
		default:
			t.Fatalf("cancel %d = %s", index, outcome)
		}
	}
	if accepted == 0 {
		t.Fatalf("neither cancel was accepted: %v", outcomes)
	}
	<-finished
	if got := kills.Load(); got != 1 {
		t.Fatalf("process cancellations = %d", got)
	}
	final, _ := engine.Get(record.OperationID)
	if final.Status != StatusCanceled {
		t.Fatalf("status = %s", final.Status)
	}
	if final.CancelReason != CancelReasonUser && final.CancelReason != CancelReasonTimeout {
		t.Fatalf("cancel reason = %q", final.CancelReason)
	}
	// Repeating either cancel stays idempotent and never changes the reason.
	reason := final.CancelReason
	for _, again := range []CancelReason{CancelReasonUser, CancelReasonTimeout} {
		if outcome := engine.Cancel(record.OperationID, again); outcome != CancelAlreadyTerminal {
			t.Fatalf("repeated cancel = %s", outcome)
		}
	}
	after, _ := engine.Get(record.OperationID)
	if after.CancelReason != reason || after.Status != StatusCanceled {
		t.Fatalf("terminal record changed: %+v", after)
	}
}

// Cancel and commit contend for the same operation. Exactly one wins, and the
// loser is told which side it lost to.
func TestCancelAndCommitRaceHasExactlyOneWinner(t *testing.T) {
	for attempt := range 200 {
		engine := NewDefault()
		operation, _, err := engine.Create(context.Background(), Spec{
			OperationID: "commit-race", ProjectKey: "project-c", Type: TypeBackupRestore,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := operation.TransitionStatus(StatusDispatched, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := operation.AdvancePhase(PhaseExecuting); err != nil {
			t.Fatal(err)
		}
		var commitErr error
		var outcome CancelOutcome
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); commitErr = operation.EnterCommit() }()
		go func() { defer group.Done(); outcome = operation.Cancel(CancelReasonUser) }()
		group.Wait()

		switch {
		case commitErr == nil && outcome == CancelTooLate:
		case commitErr != nil && outcome == CancelAccepted:
		default:
			t.Fatalf("attempt %d: both sides won: commit=%v cancel=%s", attempt, commitErr, outcome)
		}
		record := operation.Snapshot()
		if outcome == CancelAccepted && record.Phase == PhaseCommitting {
			t.Fatalf("attempt %d: cancel accepted after commit entered", attempt)
		}
		if commitErr == nil && !record.CancelRequestedAt.IsZero() {
			t.Fatalf("attempt %d: commit entered after a cancel was accepted", attempt)
		}
	}
}

func waitForTerminal(t *testing.T, engine *Engine, operationID string) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, found := engine.Get(operationID)
		if found && record.Status.Terminal() {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %q did not reach a terminal state: %+v", operationID, record)
		}
		time.Sleep(time.Millisecond)
	}
}
