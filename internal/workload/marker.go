package workload

// This package is intentionally test/prototype-only. The unexported marker is
// referenced from Service to make copying it into production code conspicuous.
type prototypeOnly struct{}
