package operation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func durableEngine(t *testing.T, state string, clock Clock, admitter PersistenceAdmitter) *Engine {
	t.Helper()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewFileJournal(state, admitter)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Clock = clock
	config.ProjectLockWait = 0
	config.Journal = journal
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestDurableJournalRecoversNonterminalAsInterruptedPartial(t *testing.T) {
	state := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)}
	engine := durableEngine(t, state, clock, nil)
	op := create(t, engine, "restore-running", TypeBackupRestore, "project")
	start(t, op)
	if err := op.EnterCommit(); err != nil {
		t.Fatal(err)
	}
	if _, err := op.WriteOutput([]byte("last useful output")); err != nil {
		t.Fatal(err)
	}
	if err := op.FlushOutputTail(); err != nil {
		t.Fatal(err)
	}
	before := op.Snapshot()
	clock.Advance(time.Second)

	restarted := durableEngine(t, state, clock, nil)
	recovered, ok := restarted.Get("restore-running")
	if !ok {
		t.Fatal("recovered operation missing")
	}
	if recovered.Status != StatusInterrupted || recovered.Phase != PhaseCommitting || !recovered.PartialEffectsPossible ||
		recovered.Revision != before.Revision+1 || recovered.FinishedAt.IsZero() || recovered.StalledWarning {
		t.Fatalf("recovered=%#v before=%#v", recovered, before)
	}
	if string(recovered.OutputTail) != "last useful output" {
		t.Fatalf("output tail=%q", recovered.OutputTail)
	}
	if active := restarted.ListActiveOperations(); len(active) != 0 {
		t.Fatalf("recovered nonterminal remains active: %#v", active)
	}
	assertJournalModes(t, state)
}

func TestDurableJournalPreservesTerminalResultAcrossRestart(t *testing.T) {
	state := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)}
	engine := durableEngine(t, state, clock, nil)
	op := create(t, engine, "done", TypeDiscoveryRescan, "")
	start(t, op)
	if err := op.AdvancePhase(PhaseFinalizing); err != nil {
		t.Fatal(err)
	}
	if err := op.TransitionStatus(StatusSuccess, "complete", ""); err != nil {
		t.Fatal(err)
	}
	restarted := durableEngine(t, state, clock, nil)
	record, ok := restarted.Get("done")
	if !ok || record.Status != StatusSuccess || record.Result != "complete" {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
}

func TestListActiveOperationsIsSortedAndCopySafe(t *testing.T) {
	engine := testEngine(t, nil)
	create(t, engine, "z-active", TypeDiscoveryRescan, "")
	first := create(t, engine, "a-active", TypeDiscoveryRescan, "")
	if _, err := first.WriteOutput([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	done := create(t, engine, "m-done", TypeDiscoveryRescan, "")
	fail(t, done)
	records := engine.ListActiveOperations()
	if len(records) != 2 || records[0].OperationID != "a-active" || records[1].OperationID != "z-active" {
		t.Fatalf("records=%#v", records)
	}
	records[0].OutputTail[0] = 'X'
	record, _ := engine.Get("a-active")
	if string(record.OutputTail) != "tail" {
		t.Fatalf("active snapshot aliased output: %q", record.OutputTail)
	}
}

func TestCommittingStalledWarningUsesProgressRevisionTime(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)}
	engine := testEngine(t, func(config *Config) {
		config.Clock = clock
		config.StalledAfter = 5 * time.Minute
	})
	op := create(t, engine, "restore", TypeBackupRestore, "project")
	start(t, op)
	if err := op.EnterCommit(); err != nil {
		t.Fatal(err)
	}
	commitRevision := op.Snapshot().Revision
	clock.Advance(5*time.Minute - time.Nanosecond)
	if op.Snapshot().StalledWarning {
		t.Fatal("warning fired before threshold")
	}
	if _, err := op.WriteOutput([]byte("noise")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Nanosecond)
	if !op.Snapshot().StalledWarning {
		t.Fatal("stdout incorrectly prevented stalled warning")
	}
	if err := op.AdvanceProgress(); err != nil {
		t.Fatal(err)
	}
	progress := op.Snapshot()
	if progress.StalledWarning || progress.Revision != commitRevision+1 {
		t.Fatalf("progress=%#v", progress)
	}
	clock.Advance(5 * time.Minute)
	if !op.Snapshot().StalledWarning {
		t.Fatal("warning did not reappear after progress threshold")
	}
}

func TestStartOperationReturnsAcceptedBeforeDetachedRunnerCompletes(t *testing.T) {
	engine := testEngine(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	accepted, created, err := engine.StartOperation(ctx, Spec{OperationID: "async", Type: TypeDiscoveryRescan}, func(runCtx context.Context, operation *Operation) {
		defer close(done)
		if runCtx.Err() != nil {
			t.Errorf("runner inherited transport cancellation: %v", runCtx.Err())
		}
		close(started)
		<-release
		if err := operation.TransitionStatus(StatusRunning, "", ""); err != nil {
			t.Errorf("running: %v", err)
			return
		}
		if err := operation.AdvancePhase(PhaseExecuting); err != nil {
			t.Errorf("executing: %v", err)
			return
		}
		if err := operation.AdvancePhase(PhaseFinalizing); err != nil {
			t.Errorf("finalizing: %v", err)
			return
		}
		if err := operation.TransitionStatus(StatusSuccess, "done", ""); err != nil {
			t.Errorf("success: %v", err)
		}
	})
	if err != nil || !created || accepted.Status != StatusRequested || accepted.Revision != 1 {
		t.Fatalf("accepted=%#v created=%v err=%v", accepted, created, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not finish")
	}
	record, _ := engine.Get("async")
	if record.Status != StatusSuccess {
		t.Fatalf("record=%#v", record)
	}
}

type admissionRecorder struct {
	mu          sync.Mutex
	classes     []PersistenceClass
	denyOutputs bool
}

func (admission *admissionRecorder) AdmitOperationPersistence(_ context.Context, request PersistenceAdmission) error {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	admission.classes = append(admission.classes, request.Class)
	if request.Class == PersistenceOutput && admission.denyOutputs {
		return errors.New("output is outside reserve")
	}
	return nil
}

func TestOutputAdmissionCanDropTailWithoutLosingMinimalRecord(t *testing.T) {
	state := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)}
	admission := &admissionRecorder{denyOutputs: true}
	engine := durableEngine(t, state, clock, admission)
	op := create(t, engine, "pressure", TypeDiscoveryRescan, "")
	if _, err := op.WriteOutput([]byte("optional")); err != nil {
		t.Fatal(err)
	}
	if err := op.FlushOutputTail(); !errors.Is(err, ErrOutputPersistenceDropped) {
		t.Fatalf("flush error=%v", err)
	}
	clock.Advance(time.Second)
	restarted := durableEngine(t, state, clock, admission)
	record, ok := restarted.Get("pressure")
	if !ok || record.Status != StatusInterrupted || len(record.OutputTail) != 0 {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if len(admission.classes) < 3 || admission.classes[0] != PersistenceMinimal || admission.classes[1] != PersistenceOutput {
		t.Fatalf("admission classes=%v", admission.classes)
	}
}

type failingJournal struct {
	mu      sync.Mutex
	records map[string]Record
	fail    error
}

func (journal *failingJournal) Load() ([]Record, error) { return nil, nil }
func (journal *failingJournal) Delete(string) error     { return nil }
func (journal *failingJournal) Save(record Record, _ bool) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.fail != nil {
		return journal.fail
	}
	journal.records[record.OperationID] = record
	return nil
}

func TestCommitAndCancelAreNotAcceptedWhenMinimalPersistenceFails(t *testing.T) {
	persistence := &failingJournal{records: make(map[string]Record)}
	engine := testEngine(t, func(config *Config) { config.Journal = persistence })
	op := create(t, engine, "restore", TypeBackupRestore, "project")
	start(t, op)
	injected := errors.New("fsync failed")
	persistence.mu.Lock()
	persistence.fail = injected
	persistence.mu.Unlock()
	if err := op.EnterCommit(); !errors.Is(err, injected) {
		t.Fatalf("commit error=%v", err)
	}
	if record := op.Snapshot(); record.Phase != PhaseExecuting || !record.CommitStartedAt.IsZero() {
		t.Fatalf("failed commit mutated record: %#v", record)
	}
	outcome, err := engine.CancelWithError("restore", CancelReasonUser)
	if outcome != CancelNotFound || !errors.Is(err, injected) {
		t.Fatalf("cancel outcome=%s err=%v", outcome, err)
	}
	if record := op.Snapshot(); !record.CancelRequestedAt.IsZero() {
		t.Fatalf("failed cancel mutated record: %#v", record)
	}
}

func assertJournalModes(t *testing.T, state string) {
	t.Helper()
	directory := filepath.Join(state, "operations")
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("journal directory mode=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("journal file %s mode=%v err=%v", entry.Name(), info, err)
		}
	}
}
