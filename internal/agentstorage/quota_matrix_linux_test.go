//go:build linux

package agentstorage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/east-true/docklattice/internal/diskbudget"
	"github.com/east-true/docklattice/internal/operation"
	"golang.org/x/sys/unix"
)

// The Phase 6 quota/free-space fault matrix runs the real statfs/WalkDir
// observer against a disposable size-limited tmpfs. Every other storage
// pressure test injects a synthetic Observation, so none of them prove that a
// real filesystem boundary produces the architecture's degraded reasons,
// admission rejections, emergency-reserve allowlist, or recovery hysteresis.
//
// The tmpfs is mounted inside a private user+mount namespace, so the matrix
// needs no privilege and can never consume space on the developer host
// filesystem.

const quotaMatrixChildEnv = "DOCKLATTICE_QUOTA_MATRIX_CHILD"

// Sizes are the production ratios of internal/config scaled to a filesystem
// small enough to fill deterministically. The policy semantics under test are
// the ratios and the ordering, not the production byte counts.
const (
	quotaFilesystemBytes = 64 << 20
	quotaStateBudget     = 8 << 20
	quotaEntryFree       = 16 << 20
	quotaExitFree        = 20 << 20
	quotaReserve         = 4 << 20
)

func quotaBudget() diskbudget.Config {
	return diskbudget.Config{
		StateBudgetBytes:      quotaStateBudget,
		EntryFreeBytes:        quotaEntryFree,
		EntryFreePercent:      5,
		ExitFreeBytes:         quotaExitFree,
		ExitFreePercent:       6,
		ExitStatePercent:      90,
		EmergencyReserveBytes: quotaReserve,
	}
}

func TestQuotaAndFreeSpaceFaultMatrixOnDisposableFilesystem(t *testing.T) {
	if os.Getenv(quotaMatrixChildEnv) == "1" {
		runQuotaMatrix(t)
		return
	}

	command := exec.Command("/proc/self/exe", "-test.run=^"+t.Name()+"$", "-test.v")
	command.Env = append(os.Environ(), quotaMatrixChildEnv+"=1")
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return
	}
	// A reached assertion must fail the matrix; an unavailable namespace or
	// tmpfs must not be reported as a passing gate.
	if strings.Contains(string(output), "--- FAIL") || strings.Contains(string(output), "quota matrix:") {
		t.Fatalf("disposable-filesystem matrix failed: %v\n%s", err, output)
	}
	t.Skipf("user+mount namespace with tmpfs is unavailable here: %v\n%s", err, output)
}

func runQuotaMatrix(t *testing.T) {
	root := mountDisposableFilesystem(t)
	stateRoot := filepath.Join(root, "state")
	ballastRoot := filepath.Join(root, "ballast")
	for _, dir := range []string{stateRoot, ballastRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("quota matrix: create %s: %v", dir, err)
		}
	}

	t.Run("HealthyFilesystemIsNotDegraded", func(t *testing.T) {
		controller := newQuotaController(t, stateRoot)
		state := refresh(t, controller)
		if state.Degraded || state.Reason != diskbudget.ReasonNone {
			t.Fatalf("quota matrix: empty filesystem reported %+v", state)
		}
		if state.EntryFreeFloor != quotaEntryFree || state.ExitFreeFloor != quotaExitFree {
			t.Fatalf("quota matrix: floors = %d/%d", state.EntryFreeFloor, state.ExitFreeFloor)
		}
	})

	t.Run("StateBudgetExceededIsReportedAlone", func(t *testing.T) {
		defer truncateDir(t, stateRoot)
		// Inside the state root, so WalkDir counts it; small enough that the
		// filesystem free floor is untouched.
		writeBallast(t, filepath.Join(stateRoot, "oversized-state"), 9<<20)
		controller := newQuotaController(t, stateRoot)
		state := refresh(t, controller)
		observation, _ := controller.Snapshot()
		if observation.AgentStateBytes <= quotaStateBudget {
			t.Fatalf("quota matrix: real WalkDir observed only %d state bytes", observation.AgentStateBytes)
		}
		if observation.FilesystemFreeBytes < quotaEntryFree {
			t.Fatalf("quota matrix: free space unexpectedly low at %d", observation.FilesystemFreeBytes)
		}
		if !state.Degraded || state.Reason != diskbudget.ReasonAgentBudgetExceeded {
			t.Fatalf("quota matrix: state budget breach reported %+v", state)
		}
	})

	t.Run("FilesystemFreeLowIsReportedAlone", func(t *testing.T) {
		defer truncateDir(t, ballastRoot)
		// Outside the state root: consumes the shared filesystem without
		// counting toward the Agent state budget.
		writeBallast(t, filepath.Join(ballastRoot, "fill"), 52<<20)
		controller := newQuotaController(t, stateRoot)
		state := refresh(t, controller)
		observation, _ := controller.Snapshot()
		if observation.FilesystemFreeBytes >= quotaEntryFree {
			t.Fatalf("quota matrix: real statfs still reports %d free", observation.FilesystemFreeBytes)
		}
		if observation.AgentStateBytes > quotaStateBudget {
			t.Fatalf("quota matrix: ballast leaked into state accounting: %d", observation.AgentStateBytes)
		}
		if !state.Degraded || state.Reason != diskbudget.ReasonFilesystemFreeLow {
			t.Fatalf("quota matrix: free-low breach reported %+v", state)
		}
	})

	t.Run("BothPressuresReportBoth", func(t *testing.T) {
		defer truncateDir(t, ballastRoot)
		defer truncateDir(t, stateRoot)
		writeBallast(t, filepath.Join(ballastRoot, "fill"), 44<<20)
		writeBallast(t, filepath.Join(stateRoot, "oversized-state"), 9<<20)
		controller := newQuotaController(t, stateRoot)
		state := refresh(t, controller)
		if !state.Degraded || state.Reason != diskbudget.ReasonBoth {
			observation, _ := controller.Snapshot()
			t.Fatalf("quota matrix: combined breach reported %+v (observation %+v)", state, observation)
		}
	})

	t.Run("DegradedAdmissionRejectsOnlyNonRecoveryWrites", func(t *testing.T) {
		defer truncateDir(t, ballastRoot)
		writeBallast(t, filepath.Join(ballastRoot, "fill"), 52<<20)
		controller := newQuotaController(t, stateRoot)
		if state := refresh(t, controller); !state.Degraded {
			t.Fatalf("quota matrix: expected degraded state, got %+v", state)
		}
		ctx := context.Background()
		if err := controller.AdmitOperation(ctx, diskbudget.OperationQuery); err != nil {
			t.Fatalf("quota matrix: read denied under pressure: %v", err)
		}
		if err := controller.AdmitOperation(ctx, diskbudget.OperationComposeRestart); err != nil {
			t.Fatalf("quota matrix: recovery mutation denied under pressure: %v", err)
		}
		if err := controller.AdmitOperation(ctx, diskbudget.OperationBackupCreate); !errors.Is(err, diskbudget.ErrStorageDegraded) {
			t.Fatalf("quota matrix: backup.create admission = %v", err)
		}
		if err := controller.AdmitOperation(ctx, diskbudget.OperationFileWrite); !errors.Is(err, diskbudget.ErrStorageDegraded) {
			t.Fatalf("quota matrix: file.write admission = %v", err)
		}
	})

	t.Run("EmergencyReserveAllowlistUsesRealFreeBytes", func(t *testing.T) {
		defer truncateDir(t, ballastRoot)
		writeBallast(t, filepath.Join(ballastRoot, "fill"), 52<<20)
		controller := newQuotaController(t, stateRoot)
		refresh(t, controller)
		observation, _ := controller.Snapshot()
		// Leaves less than the emergency reserve behind, so only a
		// reserve-eligible class may proceed.
		request := observation.FilesystemFreeBytes - (quotaReserve / 2)
		if request <= 0 {
			t.Fatalf("quota matrix: not enough free space to size the request: %d", observation.FilesystemFreeBytes)
		}
		ctx := context.Background()
		if err := controller.AdmitOperationPersistence(ctx, operation.PersistenceAdmission{
			Class: operation.PersistenceMinimal, EstimatedBytes: request,
		}); err != nil {
			t.Fatalf("quota matrix: reserve-eligible minimum denied: %v", err)
		}
		if err := controller.AdmitOperationPersistence(ctx, operation.PersistenceAdmission{
			Class: operation.PersistenceOutput, EstimatedBytes: request,
		}); !errors.Is(err, operation.ErrOutputPersistenceDropped) {
			t.Fatalf("quota matrix: non-eligible output admission = %v", err)
		}
	})

	t.Run("RecoveryRequiresExitFloorNotEntryFloor", func(t *testing.T) {
		defer truncateDir(t, ballastRoot)
		fill := filepath.Join(ballastRoot, "fill")
		writeBallast(t, fill, 52<<20)
		controller := newQuotaController(t, stateRoot)
		state := refresh(t, controller)
		if !state.Degraded || state.Reason != diskbudget.ReasonFilesystemFreeLow {
			t.Fatalf("quota matrix: expected free-low degraded entry, got %+v", state)
		}

		// Inside the hysteresis band: above the entry floor, below the exit
		// floor. The cause the operator must still resolve is preserved.
		resizeBallast(t, fill, 46<<20)
		state = refresh(t, controller)
		observation, _ := controller.Snapshot()
		if observation.FilesystemFreeBytes < quotaEntryFree || observation.FilesystemFreeBytes >= quotaExitFree {
			t.Fatalf("quota matrix: free bytes %d are not inside the hysteresis band", observation.FilesystemFreeBytes)
		}
		if !state.Degraded {
			t.Fatalf("quota matrix: recovered at the entry floor instead of the exit floor: %+v", state)
		}
		if state.Reason != diskbudget.ReasonFilesystemFreeLow {
			t.Fatalf("quota matrix: hysteresis band lost the degraded reason: %+v", state)
		}

		// Above the exit floor with the state budget satisfied.
		resizeBallast(t, fill, 20<<20)
		state = refresh(t, controller)
		observation, _ = controller.Snapshot()
		if observation.FilesystemFreeBytes < quotaExitFree {
			t.Fatalf("quota matrix: free bytes %d never cleared the exit floor", observation.FilesystemFreeBytes)
		}
		if state.Degraded || state.Reason != diskbudget.ReasonNone {
			t.Fatalf("quota matrix: did not recover above the exit floor: %+v", state)
		}
	})
}

func mountDisposableFilesystem(t *testing.T) string {
	t.Helper()
	// A fresh mount namespace can still inherit shared propagation, which would
	// leak the tmpfs back to the host mount table.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Skipf("cannot make mount namespace private: %v", err)
	}
	root := t.TempDir()
	options := fmt.Sprintf("size=%d", quotaFilesystemBytes)
	if err := unix.Mount("tmpfs", root, "tmpfs", 0, options); err != nil {
		t.Skipf("cannot mount a disposable tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(root, unix.MNT_DETACH) })
	return root
}

func newQuotaController(t *testing.T, stateRoot string) *Controller {
	t.Helper()
	// Observe is deliberately left nil so the real statfs/WalkDir observer runs.
	controller, err := New(Config{StateRoot: stateRoot, Budget: quotaBudget()})
	if err != nil {
		t.Fatalf("quota matrix: new controller: %v", err)
	}
	return controller
}

func refresh(t *testing.T, controller *Controller) diskbudget.State {
	t.Helper()
	state, err := controller.Refresh(context.Background())
	if err != nil {
		t.Fatalf("quota matrix: refresh: %v", err)
	}
	return state
}

func writeBallast(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("quota matrix: create %s: %v", path, err)
	}
	defer file.Close()
	chunk := make([]byte, 1<<20)
	for written := int64(0); written < size; {
		remaining := size - written
		if remaining > int64(len(chunk)) {
			remaining = int64(len(chunk))
		}
		count, err := file.Write(chunk[:remaining])
		if err != nil {
			t.Fatalf("quota matrix: fill %s at %d bytes: %v", path, written, err)
		}
		written += int64(count)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("quota matrix: sync %s: %v", path, err)
	}
}

func resizeBallast(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("quota matrix: truncate %s: %v", path, err)
	}
}

func truncateDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("quota matrix: read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatalf("quota matrix: clean %s: %v", dir, err)
		}
	}
}
