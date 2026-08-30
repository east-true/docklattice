package agentstorage

import (
	"context"
	"errors"
	"testing"

	"github.com/east-true/docklattice/internal/backup"
	"github.com/east-true/docklattice/internal/diskbudget"
	"github.com/east-true/docklattice/internal/operation"
)

func TestControllerAppliesDegradedPolicyAndReserveClasses(t *testing.T) {
	observation := diskbudget.Observation{FilesystemTotalBytes: 10 << 30, FilesystemFreeBytes: 400 << 20, AgentStateBytes: 100}
	controller, err := New(Config{StateRoot: t.TempDir(), Observe: func(context.Context, string) (diskbudget.Observation, error) {
		return observation, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.AdmitOperation(context.Background(), diskbudget.OperationComposeRestart); err != nil {
		t.Fatalf("degraded recovery mutation denied: %v", err)
	}
	if err := controller.AdmitOperation(context.Background(), diskbudget.OperationBackupCreate); !errors.Is(err, diskbudget.ErrStorageDegraded) {
		t.Fatalf("degraded backup error = %v", err)
	}
	if err := controller.AdmitOperationPersistence(context.Background(), operation.PersistenceAdmission{
		Class: operation.PersistenceMinimal, EstimatedBytes: 1 << 20,
	}); err != nil {
		t.Fatalf("reserve-eligible minimum denied: %v", err)
	}
	if err := controller.AdmitOperationPersistence(context.Background(), operation.PersistenceAdmission{
		Class: operation.PersistenceOutput, EstimatedBytes: 400 << 20,
	}); !errors.Is(err, operation.ErrOutputPersistenceDropped) {
		t.Fatalf("output reserve error = %v", err)
	}
}

func TestControllerBackupAndRestoreUseCorrectFilesystems(t *testing.T) {
	observation := diskbudget.Observation{FilesystemTotalBytes: 100 << 30, FilesystemFreeBytes: 10 << 30, AgentStateBytes: 1 << 20}
	controller, err := New(Config{
		StateRoot: t.TempDir(), Observe: func(context.Context, string) (diskbudget.Observation, error) {
			return observation, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.AdmitBackup(context.Background(), backup.Admission{Trigger: backup.TriggerManual, EstimatedBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := controller.AdmitRestore(context.Background(), backup.RestoreAdmission{
		FilesystemTotalBytes: 100 << 30, FilesystemFreeBytes: 10 << 30, EstimatedBytes: 11 << 30,
	}); !errors.Is(err, ErrProjectFilesystemAdmission) || !errors.Is(err, diskbudget.ErrStorageDegraded) {
		t.Fatalf("oversized restore error = %v", err)
	}
	if err := controller.AdmitProjectStaging(context.Background(), 100<<30, 10<<30, 4<<30); err != nil {
		t.Fatalf("project staging with headroom denied: %v", err)
	}
}

func TestRealObserverCountsStateAndReportsFilesystem(t *testing.T) {
	root := t.TempDir()
	controller, err := New(Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	state, err := controller.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observation, snapshot := controller.Snapshot()
	if observation.FilesystemTotalBytes <= 0 || observation.FilesystemFreeBytes <= 0 || state != snapshot {
		t.Fatalf("observation=%+v state=%+v snapshot=%+v", observation, state, snapshot)
	}
}
