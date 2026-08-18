package operation

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

type recordingTerminalAuditor struct {
	mu           sync.Mutex
	deliveries   map[string]int
	confirms     map[string]int
	failDelivery error
	inspect      func(Record)
}

func (auditor *recordingTerminalAuditor) DeliverTerminal(_ context.Context, record Record) error {
	if auditor.inspect != nil {
		auditor.inspect(record)
	}
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	if auditor.failDelivery != nil {
		return auditor.failDelivery
	}
	auditor.deliveries[record.OperationID]++
	return nil
}

func (auditor *recordingTerminalAuditor) ConfirmTerminal(_ context.Context, operationID string) error {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	auditor.confirms[operationID]++
	return nil
}

func TestTerminalAuditPendingIsDurableBeforeDelivery(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	auditor := &recordingTerminalAuditor{deliveries: make(map[string]int), confirms: make(map[string]int)}
	auditor.inspect = func(record Record) {
		records, loadErr := journal.Load()
		if loadErr != nil {
			t.Errorf("load terminal journal: %v", loadErr)
			return
		}
		if len(records) != 1 || records[0].Status != StatusFailed || records[0].ManagedAuditDelivery != ManagedAuditPending {
			t.Errorf("audit ran before pending terminal journal was durable: %#v", records)
		}
		if record.ManagedAuditDelivery != ManagedAuditPending {
			t.Errorf("delivered record state=%q", record.ManagedAuditDelivery)
		}
	}
	config := DefaultConfig()
	config.Clock, config.Journal, config.TerminalAuditor = clock, journal, auditor
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	op := create(t, engine, "terminal-order", TypeDiscoveryRescan, "")
	fail(t, op)
	record := op.Snapshot()
	if record.ManagedAuditDelivery != ManagedAuditDelivered {
		t.Fatalf("delivery state=%q", record.ManagedAuditDelivery)
	}
	if auditor.deliveries[record.OperationID] != 1 || auditor.confirms[record.OperationID] != 1 {
		t.Fatalf("deliveries=%v confirms=%v", auditor.deliveries, auditor.confirms)
	}
}

func TestFailedTerminalAuditRemainsPendingAndRestartReplays(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	auditor := &recordingTerminalAuditor{
		deliveries: make(map[string]int), confirms: make(map[string]int), failDelivery: errors.New("WAL unavailable"),
	}
	config := DefaultConfig()
	config.Clock, config.Journal, config.TerminalAuditor = clock, journal, auditor
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	op := create(t, engine, "replay", TypeDiscoveryRescan, "")
	fail(t, op)
	if pending := engine.PendingTerminalAudits(); len(pending) != 1 || pending[0].OperationID != "replay" {
		t.Fatalf("pending=%#v", pending)
	}

	auditor.mu.Lock()
	auditor.failDelivery = nil
	auditor.mu.Unlock()
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if pending := restarted.PendingTerminalAudits(); len(pending) != 0 {
		t.Fatalf("pending after restart=%#v", pending)
	}
	record, ok := restarted.Get("replay")
	if !ok || record.ManagedAuditDelivery != ManagedAuditDelivered {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
	if auditor.deliveries["replay"] != 1 || auditor.confirms["replay"] != 1 {
		t.Fatalf("deliveries=%v confirms=%v", auditor.deliveries, auditor.confirms)
	}
}

func TestPendingTerminalAuditIsNotEvicted(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)}
	auditor := &recordingTerminalAuditor{
		deliveries: make(map[string]int), confirms: make(map[string]int), failDelivery: errors.New("offline"),
	}
	config := DefaultConfig()
	config.Clock, config.Journal, config.TerminalAuditor, config.ResultMax, config.ResultRetention = clock, journal, auditor, 1, time.Minute
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	first := create(t, engine, "pending-oldest", TypeDiscoveryRescan, "")
	fail(t, first)
	clock.Advance(2 * time.Minute)
	second := create(t, engine, "pending-newer", TypeDiscoveryRescan, "")
	fail(t, second)
	if _, ok := engine.Get("pending-oldest"); !ok {
		t.Fatal("pending terminal audit was evicted")
	}
}

func TestTerminalAuditorRequiresDurableJournal(t *testing.T) {
	config := DefaultConfig()
	config.TerminalAuditor = &recordingTerminalAuditor{deliveries: make(map[string]int), confirms: make(map[string]int)}
	if _, err := New(config); !HasErrorCode(err, CodeInvalidSpec) {
		t.Fatalf("New error=%v", err)
	}
}

func TestRestartMarksInterruptedAndAuditsThatDurableTerminal(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)}
	journal, err := NewFileJournal(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := DefaultConfig()
	base.Clock, base.Journal = clock, journal
	engine, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	op := create(t, engine, "interrupted-audit", TypeBackupRestore, "project")
	start(t, op)
	if err := op.EnterCommit(); err != nil {
		t.Fatal(err)
	}

	auditor := &recordingTerminalAuditor{deliveries: make(map[string]int), confirms: make(map[string]int)}
	auditor.inspect = func(record Record) {
		if record.Status != StatusInterrupted || !record.PartialEffectsPossible || record.ManagedAuditDelivery != ManagedAuditPending {
			t.Errorf("replayed record=%#v", record)
		}
	}
	base.TerminalAuditor = auditor
	restarted, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := restarted.Get("interrupted-audit")
	if !ok || record.Status != StatusInterrupted || record.ManagedAuditDelivery != ManagedAuditDelivered {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
	if auditor.deliveries[record.OperationID] != 1 {
		t.Fatalf("deliveries=%v", auditor.deliveries)
	}
}
