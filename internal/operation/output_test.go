package operation

import (
	"context"
	"testing"
)

func TestOutputTailAlwaysDrainsAndIsBounded(t *testing.T) {
	engine := testEngine(t, func(config *Config) { config.OutputTailBytes = 8 })
	operation := create(t, engine, "output", TypeDiscoveryRescan, "")
	for _, payload := range [][]byte{[]byte("abc"), []byte("defghij")} {
		written, err := operation.WriteOutput(payload)
		if err != nil || written != len(payload) {
			t.Fatalf("WriteOutput() = %d, %v; want %d, nil", written, err, len(payload))
		}
	}
	record := operation.Snapshot()
	if got := string(record.OutputTail); got != "cdefghij" || !record.OutputTruncated {
		t.Fatalf("tail=%q truncated=%v", got, record.OutputTruncated)
	}

	large := []byte("0123456789abcdefghijklmnop")
	written, err := operation.WriteOutput(large)
	if err != nil || written != len(large) {
		t.Fatalf("large WriteOutput() = %d, %v", written, err)
	}
	record = operation.Snapshot()
	if got := string(record.OutputTail); got != "ijklmnop" || !record.OutputTruncated {
		t.Fatalf("large tail=%q truncated=%v", got, record.OutputTruncated)
	}

	// Snapshot callers cannot mutate the retained tail.
	record.OutputTail[0] = 'X'
	if got := string(operation.Snapshot().OutputTail); got != "ijklmnop" {
		t.Fatalf("snapshot alias changed retained tail to %q", got)
	}
}

func TestExactOutputLimitDoesNotClaimTruncation(t *testing.T) {
	config := DefaultConfig()
	config.OutputTailBytes = 4
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err := engine.Create(context.Background(), Spec{OperationID: "exact", Type: TypeDiscoveryRescan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.WriteOutput([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	record := operation.Snapshot()
	if string(record.OutputTail) != "1234" || record.OutputTruncated {
		t.Fatalf("record = %#v", record)
	}
}
