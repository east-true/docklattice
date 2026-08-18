package agentstorage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/diskbudget"
)

type fakeProjectSnapshot []agentprojects.Project

func (snapshot fakeProjectSnapshot) Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus) {
	return append([]agentprojects.Project(nil), snapshot...), agentprojects.ScanStatus{}
}

type fakeEvictionWAL struct {
	order      *[]diskbudget.Tier
	acked      int64
	unacked    int64
	ackedErr   error
	unackedErr error
	temp       int64
}

func (fake *fakeEvictionWAL) ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error) {
	return fake.temp, nil
}

func (fake *fakeEvictionWAL) ReclaimACKedForDiskPressure(int64) (auditwal.ReclaimResult, error) {
	*fake.order = append(*fake.order, diskbudget.TierACKedWAL)
	return auditwal.ReclaimResult{FreedBytes: fake.acked}, fake.ackedErr
}

func (fake *fakeEvictionWAL) ReclaimUnackedForDiskPressure(int64) (auditwal.ReclaimResult, error) {
	*fake.order = append(*fake.order, diskbudget.TierUnackedWAL)
	return auditwal.ReclaimResult{FreedBytes: fake.unacked}, fake.unackedErr
}

type fakeEvictionOperations struct {
	order   *[]diskbudget.Tier
	temp    int64
	expired int64
	enter   chan struct{}
	release chan struct{}
	once    sync.Once
	tempFn  func(int64) (int64, error)
}

func (fake *fakeEvictionOperations) ReclaimAbandonedTempForDiskPressure(_ context.Context, requested int64) (int64, error) {
	if fake.enter != nil {
		fake.once.Do(func() { close(fake.enter) })
		<-fake.release
	}
	if fake.tempFn != nil {
		return fake.tempFn(requested)
	}
	return fake.temp, nil
}

type fakeFileStaging struct {
	order *[]diskbudget.Tier
	bytes int64
}

func (fake *fakeFileStaging) ReclaimAbandonedStagingForDiskPressure(context.Context, int64) (int64, error) {
	*fake.order = append(*fake.order, diskbudget.TierAbandonedTemp)
	return fake.bytes, nil
}

func (fake *fakeEvictionOperations) ReclaimExpiredForDiskPressure(context.Context, int64) (int64, error) {
	*fake.order = append(*fake.order, diskbudget.TierExpiredOperations)
	return fake.expired, nil
}

type fakeEvictionBackups struct {
	order  *[]diskbudget.Tier
	temp   int64
	excess int64
	old    int64
}

func (fake *fakeEvictionBackups) ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error) {
	return fake.temp, nil
}

func (fake *fakeEvictionBackups) ReclaimExcessAutomaticForDiskPressure(context.Context, int64, int) (int64, error) {
	*fake.order = append(*fake.order, diskbudget.TierExcessSnapshots)
	return fake.excess, nil
}

func (fake *fakeEvictionBackups) ReclaimOldAutomaticForDiskPressure(context.Context, int64) (int64, error) {
	*fake.order = append(*fake.order, diskbudget.TierOldSnapshots)
	return fake.old, nil
}

func TestEvictionExecutorUsesExactGlobalOrderThenDegrades(t *testing.T) {
	var order []diskbudget.Tier
	wal := &fakeEvictionWAL{order: &order, acked: 1, unacked: 1}
	operations := &fakeEvictionOperations{order: &order, temp: 1, expired: 1}
	backups := &fakeEvictionBackups{order: &order, excess: 1, old: 1}
	executor, err := NewEvictionExecutor(EvictionConfig{
		WAL: wal, Operations: operations, Backups: backups, FileStaging: &fakeFileStaging{order: &order, bytes: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Reclaim(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []diskbudget.Tier{
		diskbudget.TierAbandonedTemp, diskbudget.TierACKedWAL, diskbudget.TierExpiredOperations,
		diskbudget.TierExcessSnapshots, diskbudget.TierOldSnapshots, diskbudget.TierUnackedWAL,
	}
	if !reflect.DeepEqual(order, want) || result.FreedBytes != 6 || !result.Degraded {
		t.Fatalf("order=%v result=%+v", order, result)
	}
}

func TestEvictionExecutorAccountsPartialFaultAndStops(t *testing.T) {
	fault := errors.New("injected WAL unlink fault")
	var order []diskbudget.Tier
	executor, err := NewEvictionExecutor(EvictionConfig{
		WAL:         &fakeEvictionWAL{order: &order, acked: 2, ackedErr: fault},
		Operations:  &fakeEvictionOperations{order: &order, temp: 1},
		Backups:     &fakeEvictionBackups{order: &order},
		FileStaging: &fakeFileStaging{order: &order},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Reclaim(context.Background(), 10)
	if !errors.Is(err, fault) || result.FreedBytes != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if want := []diskbudget.Tier{diskbudget.TierAbandonedTemp, diskbudget.TierACKedWAL}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order after fault=%v want %v", order, want)
	}
}

func TestEvictionExecutorSerializesConcurrentRuns(t *testing.T) {
	var order []diskbudget.Tier
	entered, release := make(chan struct{}), make(chan struct{})
	operations := &fakeEvictionOperations{order: &order, enter: entered, release: release}
	executor, err := NewEvictionExecutor(EvictionConfig{
		WAL: &fakeEvictionWAL{order: &order}, Operations: operations, Backups: &fakeEvictionBackups{order: &order},
		FileStaging: &fakeFileStaging{order: &order},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = executor.Reclaim(context.Background(), 1); done <- struct{}{} }()
	<-entered
	go func() { _, _ = executor.Reclaim(context.Background(), 1); done <- struct{}{} }()
	select {
	case <-done:
		t.Fatal("a reclaim completed while the first executor run was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	<-done
}

func TestEvictionExecutorUnackedTierCreatesDurableDiskPressureGap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	options := auditwal.DefaultOptions()
	options.SyncBytes = 1 << 20
	wal, err := auditwal.Open(dir, "agent-1", 1, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), []byte("unacked event")); err != nil {
		t.Fatal(err)
	}
	if err := wal.Sync(); err != nil {
		t.Fatal(err)
	}
	var order []diskbudget.Tier
	executor, err := NewEvictionExecutor(EvictionConfig{
		WAL: wal, Operations: &fakeEvictionOperations{order: &order}, Backups: &fakeEvictionBackups{order: &order},
		FileStaging: &fakeFileStaging{order: &order},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Reclaim(context.Background(), 1)
	if err != nil || result.FreedBytes == 0 || result.Degraded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	coverage, err := wal.GetAuditCoverage()
	if err != nil || len(coverage.Gaps) != 1 || coverage.Gaps[0].Reason != auditwal.GapDiskPressure {
		t.Fatalf("coverage=%+v err=%v", coverage, err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := auditwal.Open(dir, "agent-1", 2, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	coverage, err = reopened.GetAuditCoverage()
	if err != nil || len(coverage.Gaps) != 1 || coverage.Gaps[0].Reason != auditwal.GapDiskPressure {
		t.Fatalf("reopened coverage=%+v err=%v", coverage, err)
	}
}

func TestProjectStagingReclaimerUsesOnlyCatalogRoots(t *testing.T) {
	project := t.TempDir()
	secondProject := t.TempDir()
	stage := filepath.Join(project, ".dockpilot-stage-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	secondStage := filepath.Join(secondProject, ".dockpilot-stage-cccccccccccccccccccccccccccccccc")
	if err := os.WriteFile(stage, []byte("stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondStage, []byte("stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), ".dockpilot-stage-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	reclaimer, err := NewProjectStagingReclaimer(fakeProjectSnapshot{
		{UID: "project", WorkingDir: project}, {UID: "second", WorkingDir: secondProject},
	})
	if err != nil {
		t.Fatal(err)
	}
	freed, err := reclaimer.ReclaimAbandonedStagingForDiskPressure(context.Background(), 1<<20)
	if err != nil || freed != 0 {
		t.Fatalf("freed=%d err=%v", freed, err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog staging was not cleaned: %v", err)
	}
	if _, err := os.Lstat(secondStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second catalog staging was not cleaned after logical target was met: %v", err)
	}
	if payload, err := os.ReadFile(outside); err != nil || string(payload) != "outside" {
		t.Fatalf("outside catalog root changed: %q %v", payload, err)
	}
}

func TestControllerReclaimForPressureTargetsBothExitWatermarks(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var observationMu sync.Mutex
	observation := diskbudget.Observation{FilesystemTotalBytes: 10_000, FilesystemFreeBytes: 50, AgentStateBytes: 1_100}
	controller, err := New(Config{
		StateRoot: root,
		Budget: diskbudget.Config{
			StateBudgetBytes: 1_000, EntryFreeBytes: 100, EntryFreePercent: 5,
			ExitFreeBytes: 120, ExitFreePercent: 6, ExitStatePercent: 90, EmergencyReserveBytes: 100,
		},
		Observe: func(context.Context, string) (diskbudget.Observation, error) {
			observationMu.Lock()
			defer observationMu.Unlock()
			return observation, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []diskbudget.Tier
	operations := &fakeEvictionOperations{order: &order, tempFn: func(requested int64) (int64, error) {
		observationMu.Lock()
		defer observationMu.Unlock()
		observation.FilesystemFreeBytes += requested
		observation.AgentStateBytes -= requested
		return requested, nil
	}}
	executor, err := NewEvictionExecutor(EvictionConfig{
		WAL: &fakeEvictionWAL{order: &order}, Operations: operations,
		Backups: &fakeEvictionBackups{order: &order}, FileStaging: &fakeFileStaging{order: &order},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.ReclaimForPressure(context.Background(), executor)
	if err != nil {
		t.Fatal(err)
	}
	// Free-space recovery needs 600-50=550 bytes; state recovery needs only
	// 1100-900=200, so the larger deficit controls.
	if result.Reclaim.RequestedBytes != 550 || result.Reclaim.FreedBytes != 550 || result.AfterState.Degraded {
		t.Fatalf("pressure result=%+v", result)
	}
}
