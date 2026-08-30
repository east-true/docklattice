package managedaudit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditwal"
	"github.com/east-true/docklattice/internal/operation"
)

func TestDeliveryWritesOneUnrateLimitedManagedRecord(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	wal, err := auditwal.Open(directory, "agent-managed", 1, auditwal.Options{
		MaxBytes: 1 << 20, MaxAge: time.Hour, SyncInterval: time.Hour, SyncBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	appender, err := auditevents.NewAppender(wal)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := New(appender)
	if err != nil {
		t.Fatal(err)
	}
	record := operation.Record{
		OperationID: "compose-up-1", ProjectKey: "project-uid", Target: "web", Type: operation.TypeComposeUp,
		FinishedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC), Status: operation.StatusSuccess,
		Phase: operation.PhaseFinalizing, Revision: 6, ManagedAuditDelivery: operation.ManagedAuditPending,
	}
	if err := delivery.DeliverTerminal(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := delivery.DeliverTerminal(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	read, err := wal.ReadAuditFrom(context.Background(), auditwal.Cursor{Incarnation: 1, Seq: 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Records) != 1 {
		t.Fatalf("records=%d, want exactly one", len(read.Records))
	}
	envelope, err := auditevents.Decode(read.Records[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.OperationID != record.OperationID || envelope.ProjectUID != record.ProjectKey ||
		envelope.Event.Action != string(record.Type) || envelope.Event.Count != 1 || envelope.Event.Attributes["status"] != "success" {
		t.Fatalf("envelope=%#v", envelope)
	}
	if err := delivery.ConfirmTerminal(context.Background(), record.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
}
