package serverapi

import "testing"

// TestARecoveryBlockedProjectStaysReadOnlyEverywhere pins the rule that Codex
// caught escaping: read-only was derived at four separate call sites, and
// adding the restore-recovery reason to one of them left the other three able
// to clear the bit again. A project reported as damaged and writable in the
// same response is worse than either alone - the dashboard contradicts itself
// and operation authorization lets through work the Agent will refuse.
func TestARecoveryBlockedProjectStaysReadOnlyEverywhere(t *testing.T) {
	healthy := projectFlags{
		Managed: true, ComposeExecutable: true, FilesystemWritable: true,
	}
	if projectReadOnly(healthy, false) {
		t.Fatalf("a healthy project was derived read-only: %+v", healthy)
	}
	blocked := healthy
	blocked.RestoreRecoveryRequired = true
	for _, collision := range []bool{false, true} {
		if !projectReadOnly(blocked, collision) {
			t.Fatalf("a recovery-blocked project was derived writable (collision=%v)", collision)
		}
	}
}
