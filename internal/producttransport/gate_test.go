package producttransport

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPriorityGateProtectsP0P1FromBulkSaturation(t *testing.T) {
	gate, err := NewPriorityGate(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	releaseBulk, err := gate.Acquire(context.Background(), P3BulkInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	releaseControl, err := gate.Acquire(ctx, P0Control)
	if err != nil {
		t.Fatalf("P0 was blocked by saturated bulk: %v", err)
	}
	releaseControl()
	releaseDurable, err := gate.Acquire(ctx, P1DurableSync)
	if err != nil {
		t.Fatalf("P1 was blocked by saturated bulk: %v", err)
	}
	releaseDurable()

	bulkCtx, bulkCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer bulkCancel()
	if _, err := gate.Acquire(bulkCtx, P4DisposableLive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second bulk acquire error = %v", err)
	}
}

func TestPriorityGateP0AndP1MakeIndependentProgress(t *testing.T) {
	gate, err := NewPriorityGate(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	releaseControl, err := gate.Acquire(context.Background(), P0Control)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseControl()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	releaseDurable, err := gate.Acquire(ctx, P1DurableSync)
	if err != nil {
		t.Fatalf("P1 was blocked by saturated P0: %v", err)
	}
	releaseDurable()

	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer blockedCancel()
	if _, err := gate.Acquire(blockedCtx, P0Control); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second P0 acquire error = %v", err)
	}
}
