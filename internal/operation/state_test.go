package operation

import (
	"context"
	"sync"
	"testing"
)

func TestLegalStateAndPhaseTransitions(t *testing.T) {
	engine := testEngine(t, nil)
	operation := create(t, engine, "safe", TypeBackupCreate, "project")
	initial := operation.Snapshot()
	if initial.Status != StatusRequested || initial.Phase != PhasePreparing || initial.Revision != 1 || initial.CancelMode != CancelSafe {
		t.Fatalf("initial record = %#v", initial)
	}
	if err := operation.TransitionStatus(StatusSuccess, "", ""); !HasErrorCode(err, CodeIllegalTransition) {
		t.Fatalf("requested -> success error = %v", err)
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
	if err := operation.AdvancePhase(PhaseCommitting); !HasErrorCode(err, CodeIllegalTransition) {
		t.Fatalf("SAFE committing error = %v", err)
	}
	if err := operation.AdvancePhase(PhaseFinalizing); err != nil {
		t.Fatal(err)
	}
	if err := operation.TransitionStatus(StatusSuccess, "done", ""); err != nil {
		t.Fatal(err)
	}
	record := operation.Snapshot()
	if !record.Status.Terminal() || record.Result != "done" || record.FinishedAt.IsZero() || record.Revision != 6 {
		t.Fatalf("terminal record = %#v", record)
	}
	if err := operation.TransitionStatus(StatusFailed, "", "late"); !HasErrorCode(err, CodeIllegalTransition) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestCancellationOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		operationType Type
		prepare       func(*testing.T, *Operation)
		want          CancelOutcome
		partial       bool
	}{
		{"safe", TypeBackupCreate, nil, CancelAccepted, false},
		{"best effort", TypeComposeUp, nil, CancelAccepted, true},
		{"before commit", TypeBackupRestore, nil, CancelAccepted, false},
		{
			"too late", TypeBackupRestore,
			func(t *testing.T, operation *Operation) {
				start(t, operation)
				if err := operation.EnterCommit(); err != nil {
					t.Fatal(err)
				}
			},
			CancelTooLate, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := testEngine(t, nil)
			operation := create(t, engine, "op", tt.operationType, "project")
			if tt.prepare != nil {
				tt.prepare(t, operation)
			}
			if got := engine.Cancel("op", CancelReasonUser); got != tt.want {
				t.Fatalf("Cancel() = %s, want %s", got, tt.want)
			}
			record := operation.Snapshot()
			if tt.want == CancelAccepted {
				if record.Status == StatusCanceled || record.CancelReason != CancelReasonUser || record.CancelRequestedAt.IsZero() || record.PartialEffectsPossible != tt.partial {
					t.Fatalf("cancel-requested record = %#v", record)
				}
				if got := engine.Cancel("op", CancelReasonTimeout); got != CancelAccepted {
					t.Fatalf("second Cancel() = %s", got)
				}
				if err := operation.TransitionStatus(StatusCanceled, "", ""); err != nil {
					t.Fatal(err)
				}
				if got := engine.Cancel("op", CancelReasonTimeout); got != CancelAlreadyTerminal {
					t.Fatalf("terminal Cancel() = %s", got)
				}
			}
		})
	}
	engine := testEngine(t, nil)
	if got := engine.Cancel("missing", CancelReasonUser); got != CancelNotFound {
		t.Fatalf("missing Cancel() = %s", got)
	}

	// NOT_CANCELABLE remains part of the stable API for internal/recovered
	// records even though every currently exposed v1 mutation type has a
	// concrete cancel mode.
	engine = testEngine(t, nil)
	notCancelable := newOperation(engine, Spec{OperationID: "not-cancelable", Type: TypeBackupCreate}, CancelNone, engine.config.Clock.Now(), engine.config.OutputTailBytes)
	engine.items["not-cancelable"] = notCancelable
	if got := engine.Cancel("not-cancelable", CancelReasonUser); got != CancelNotCancelable {
		t.Fatalf("Cancel() = %s, want %s", got, CancelNotCancelable)
	}
}

func TestCommitCancelRaceIsSerialized(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		engine := testEngine(t, nil)
		operation := create(t, engine, "op", TypeBackupRestore, "project")
		start(t, operation)
		gate := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var commitErr error
		var cancelOutcome CancelOutcome
		go func() {
			defer wait.Done()
			<-gate
			commitErr = operation.EnterCommit()
		}()
		go func() {
			defer wait.Done()
			<-gate
			cancelOutcome = engine.Cancel("op", CancelReasonUser)
		}()
		close(gate)
		wait.Wait()
		record := operation.Snapshot()
		switch cancelOutcome {
		case CancelAccepted:
			if commitErr == nil || record.Status != StatusRunning || record.CancelRequestedAt.IsZero() || !record.CommitStartedAt.IsZero() {
				t.Fatalf("cancel won: commitErr=%v record=%#v", commitErr, record)
			}
			if err := operation.TransitionStatus(StatusCanceled, "", ""); err != nil {
				t.Fatal(err)
			}
		case CancelTooLate:
			if commitErr != nil || record.Phase != PhaseCommitting || record.CommitStartedAt.IsZero() {
				t.Fatalf("commit won: commitErr=%v record=%#v", commitErr, record)
			}
		default:
			t.Fatalf("unexpected cancel outcome %s", cancelOutcome)
		}
	}
}

func TestBeforeCommitPhaseSequence(t *testing.T) {
	engine := testEngine(t, nil)
	operation := create(t, engine, "restore", TypeBackupRestore, "project")
	start(t, operation)
	if err := operation.AdvancePhase(PhaseFinalizing); !HasErrorCode(err, CodeIllegalTransition) {
		t.Fatalf("skipped commit error = %v", err)
	}
	if err := operation.EnterCommit(); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(PhaseFinalizing); err != nil {
		t.Fatal(err)
	}
	if err := operation.TransitionStatus(StatusSuccess, "restored", ""); err != nil {
		t.Fatal(err)
	}
}

func TestStatusUnknownCanReconcile(t *testing.T) {
	engine := testEngine(t, nil)
	operation := create(t, engine, "unknown", TypeDiscoveryRescan, "")
	if err := operation.TransitionStatus(StatusDispatched, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := operation.TransitionStatus(StatusUnknown, "", "transport lost"); err != nil {
		t.Fatal(err)
	}
	if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := operation.Snapshot().Status; got != StatusRunning {
		t.Fatalf("status = %s", got)
	}
}

func TestCancelDoesNotDependOnCallerContext(t *testing.T) {
	engine := testEngine(t, nil)
	operation := create(t, engine, "op", TypeComposeUp, "project")
	start(t, operation)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx // A Browser context ending does not call Engine.Cancel.
	if got := operation.Snapshot().Status; got != StatusRunning {
		t.Fatalf("browser cancellation changed operation status to %s", got)
	}
}
