package diskbudget

import (
	"context"
	"errors"
	"reflect"
	"testing"

	productconfig "github.com/east-true/docklattice/internal/config"
)

func TestDefaultThresholdsAndHysteresis(t *testing.T) {
	config := DefaultConfig()
	defaults := productconfig.V1Defaults()
	if config.StateBudgetBytes != defaults.AgentStateMaxBytes || config.EmergencyReserveBytes != defaults.EmergencyReserveBytes || config.ExitStatePercent != 90 {
		t.Fatalf("defaults = %+v", config)
	}
	manager, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	total := int64(40) * productconfig.GiB // percentage floors dominate.
	state, err := manager.Observe(Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: 3 * productconfig.GiB, AgentStateBytes: productconfig.GiB})
	if err != nil || state.Degraded {
		t.Fatalf("healthy = %+v, %v", state, err)
	}
	state, _ = manager.Observe(Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: 2*productconfig.GiB - 1, AgentStateBytes: productconfig.GiB})
	if !state.Degraded || state.Reason != ReasonFilesystemFreeLow {
		t.Fatalf("entry = %+v", state)
	}
	state, _ = manager.Observe(Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: 2*productconfig.GiB + 1, AgentStateBytes: productconfig.GiB})
	if !state.Degraded || state.Reason != ReasonFilesystemFreeLow {
		t.Fatalf("hysteresis band = %+v", state)
	}
	state, _ = manager.Observe(Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: 3 * productconfig.GiB, AgentStateBytes: defaults.AgentStateMaxBytes * 90 / 100})
	if state.Degraded || state.Reason != ReasonNone {
		t.Fatalf("exit = %+v", state)
	}
}

func TestReasonsDistinguishBudgetFreeAndBoth(t *testing.T) {
	total := int64(100) * productconfig.GiB
	for _, test := range []struct {
		free, state int64
		want        DegradedReason
	}{
		{4 * productconfig.GiB, productconfig.GiB, ReasonFilesystemFreeLow},
		{10 * productconfig.GiB, 3 * productconfig.GiB, ReasonAgentBudgetExceeded},
		{4 * productconfig.GiB, 3 * productconfig.GiB, ReasonBoth},
	} {
		fresh, _ := New(DefaultConfig())
		state, err := fresh.Observe(Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: test.free, AgentStateBytes: test.state})
		if err != nil || state.Reason != test.want {
			t.Errorf("reason = %+v,%v want %s", state, err, test.want)
		}
	}
}

func TestDegradedAdmissionMatrix(t *testing.T) {
	state := State{Degraded: true, Reason: ReasonBoth}
	for _, operation := range []Operation{OperationQuery, OperationFileRead, OperationLogs, OperationMetrics, OperationAuditSync, OperationResultRead, OperationBackupList, OperationBackupDelete} {
		if got := Admit(state, operation, false); !got.Allowed {
			t.Errorf("%s unexpectedly denied: %+v", operation, got)
		}
	}
	for _, operation := range []Operation{OperationComposeUp, OperationComposeDown, OperationComposeStart, OperationComposeStop, OperationComposeRestart, OperationContainerStart, OperationContainerStop, OperationContainerRestart, OperationContainerRemove} {
		if got := Admit(state, operation, false); got.Allowed || !errors.Is(got.Err, ErrDurableAdmission) {
			t.Errorf("%s without durable capacity = %+v", operation, got)
		}
		if got := Admit(state, operation, true); !got.Allowed {
			t.Errorf("%s with durable capacity = %+v", operation, got)
		}
	}
	for _, operation := range []Operation{OperationComposePull, OperationFileWrite, OperationBackupCreate, OperationBackupRestore, OperationAutomaticSnapshot} {
		if got := Admit(state, operation, true); got.Allowed || !errors.Is(got.Err, ErrStorageDegraded) {
			t.Errorf("%s = %+v", operation, got)
		}
	}
}

func TestEmergencyReserveAllowlist(t *testing.T) {
	manager, _ := New(DefaultConfig())
	free := int64(70) << 20
	bytes := int64(10) << 20
	for _, class := range []WriteClass{WriteAuditWAL, WriteAuditCoverage, WriteContinuity, WriteOperationMinimum, WriteRestoreJournal, WriteAgentLifecycle} {
		if !manager.CanWrite(free, bytes, class) {
			t.Errorf("essential class %s denied", class)
		}
	}
	for _, class := range []WriteClass{WriteOperationOutput, WriteLogs, WriteMetrics, WriteBackup, WriteAutomaticSnapshot, WriteStaging} {
		if manager.CanWrite(free, bytes, class) {
			t.Errorf("nonessential class %s consumed reserve", class)
		}
	}
}

type fakeReclaimer struct {
	tier  Tier
	order *[]Tier
	bytes int64
	err   error
}

func (f fakeReclaimer) Reclaim(_ context.Context, requested int64) (int64, error) {
	*f.order = append(*f.order, f.tier)
	if f.bytes > requested {
		return requested, nil
	}
	return f.bytes, f.err
}

func TestReclaimPreservesPartialByteAccountingOnFault(t *testing.T) {
	fault := errors.New("injected unlink fault")
	var order []Tier
	result, err := Reclaim(context.Background(), 10, Reclaimers{
		AbandonedTemp: fakeReclaimer{tier: TierAbandonedTemp, order: &order, bytes: 2},
		ACKedWAL:      fakeReclaimer{tier: TierACKedWAL, order: &order, bytes: 3, err: fault},
	})
	if !errors.Is(err, fault) || result.FreedBytes != 5 || len(result.Steps) != 2 || result.Steps[1].FreedBytes != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReclaimUsesFixedOrderAndHasNoManualBackupTier(t *testing.T) {
	var order []Tier
	makeTier := func(tier Tier, bytes int64) Reclaimer { return fakeReclaimer{tier: tier, order: &order, bytes: bytes} }
	result, err := Reclaim(context.Background(), 55, Reclaimers{
		AbandonedTemp: makeTier(TierAbandonedTemp, 10), ACKedWAL: makeTier(TierACKedWAL, 10),
		ExpiredOperations: makeTier(TierExpiredOperations, 10), ExcessSnapshots: makeTier(TierExcessSnapshots, 10),
		OldSnapshots: makeTier(TierOldSnapshots, 10), UnackedWAL: makeTier(TierUnackedWAL, 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Tier{TierAbandonedTemp, TierACKedWAL, TierExpiredOperations, TierExcessSnapshots, TierOldSnapshots, TierUnackedWAL}
	if !reflect.DeepEqual(order, want) || result.FreedBytes != 55 || result.Degraded {
		t.Fatalf("order=%v result=%+v", order, result)
	}
	result, err = Reclaim(context.Background(), 100, Reclaimers{AbandonedTemp: makeTier(TierAbandonedTemp, 1)})
	if err != nil || !result.Degraded {
		t.Fatalf("insufficient result=%+v err=%v", result, err)
	}
}
