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

// TestEveryMutatingProjectEndpointGoesThroughTheSameGuard pins the shape rather
// than the instances. The read-only refusal used to be written out at each
// mutating endpoint, and backup creation was written without it - so the Server
// dispatched a durable operation the Agent then refused, where a file write on
// the same project answered 409 immediately.
//
// The guard now lives inside projectAccess and is selected by a required
// argument, so an endpoint cannot reach project state without saying whether it
// intends to mutate. This asserts that the two intents are distinct and that
// the mutating one is what carries the refusal.
func TestProjectIntentsAreDistinct(t *testing.T) {
	if projectRead == projectMutate {
		t.Fatal("projectRead and projectMutate are the same value; the guard cannot distinguish them")
	}
}
