package scheduling

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/transport"
)

func TestWeightedPolicyProtectsP0AndP1(t *testing.T) {
	a := New[transport.Class](64)
	// Queue work before Run so selection, rather than arrival timing, decides.
	for i := 0; i < 32; i++ {
		if err := a.Enqueue(context.Background(), transport.ClassBulk, transport.ClassBulk); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		if err := a.Enqueue(context.Background(), transport.ClassControl, transport.ClassControl); err != nil {
			t.Fatal(err)
		}
		if err := a.Enqueue(context.Background(), transport.ClassDurable, transport.ClassDurable); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var got []transport.Class
	done := make(chan error, 1)
	go func() {
		done <- a.Run(func(class transport.Class) error {
			mu.Lock()
			got = append(got, class)
			count := len(got)
			mu.Unlock()
			if count == 24 {
				a.Stop()
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("arbiter did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	control, durable := 0, 0
	for _, class := range got[:16] {
		if class == transport.ClassControl {
			control++
		}
		if class == transport.ClassDurable {
			durable++
		}
	}
	if control == 0 || durable == 0 {
		t.Fatalf("protected classes starved in first cycle: %v", got[:16])
	}
}

func TestQueueIsBounded(t *testing.T) {
	a := New[int](1)
	if err := a.Enqueue(context.Background(), transport.ClassBulk, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := a.Enqueue(ctx, transport.ClassBulk, 2); transport.StatusOf(err).Code != transport.CodeDeadlineExceeded {
		t.Fatalf("bounded enqueue error = %v", err)
	}
}
