package auditgen

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

func TestManagedIsNeverRateLimitedAndValidatesActor(t *testing.T) {
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 100; index++ {
		event, err := Managed(Signal{ResourceType: "operation", ResourceID: fmt.Sprint(index), Action: "completed", OccurredAt: at}, "ui:127.0.0.1")
		if err != nil || event.Kind != KindManaged || event.Count != 1 {
			t.Fatalf("event %d = %+v, %v", index, event, err)
		}
	}
	if _, err := Managed(Signal{ResourceType: "operation", ResourceID: "1", Action: "completed", OccurredAt: at}, "user:invented"); err == nil {
		t.Fatal("invented actor accepted")
	}
}

func TestObservedWhitelistAndFiveSecondCoalescing(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	config := DefaultConfig()
	config.Clock = clock
	generator, _ := New(config)
	for index := 0; index < 3; index++ {
		clock.now = base.Add(time.Duration(index) * time.Second)
		out, err := generator.Observe(Signal{ResourceType: "container", ResourceID: "c1", Action: "health_status: healthy", OccurredAt: clock.now})
		if err != nil || len(out) != 0 {
			t.Fatalf("observe = %v, %v", out, err)
		}
	}
	clock.now = base.Add(5 * time.Second)
	out := generator.Flush(clock.now)
	if len(out) != 1 || out[0].Kind != KindObserved || out[0].Action != "health_status" || out[0].Count != 3 || out[0].Actor != "" {
		t.Fatalf("coalesced = %+v", out)
	}
	out, err := generator.Observe(Signal{ResourceType: "container", ResourceID: "c1", Action: "exec_start", OccurredAt: clock.now})
	if err != nil || len(out) != 0 || generator.Pending() != 0 {
		t.Fatalf("excluded event = %+v, %v pending=%d", out, err, generator.Pending())
	}
}

func TestRateLimitProducesOneStormSummary(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	config := DefaultConfig()
	config.Clock, config.CoalescingWindow, config.MaxEventsPerSecond, config.MaxPending = clock, time.Second, 2, 10
	generator, _ := New(config)
	for index := 0; index < 5; index++ {
		_, err := generator.Observe(Signal{ResourceType: "container", ResourceID: fmt.Sprintf("c%d", index), Action: "start", OccurredAt: base})
		if err != nil {
			t.Fatal(err)
		}
	}
	clock.now = base.Add(time.Second)
	out := generator.Flush(clock.now)
	if len(out) != 3 || out[2].Kind != KindEventStorm || out[2].Count != 3 {
		t.Fatalf("first flush = %+v", out)
	}
}

func TestPendingSetIsBoundedAndOverflowJoinsStorm(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	config := DefaultConfig()
	config.Clock, config.MaxPending = clock, 2
	generator, _ := New(config)
	for index := 0; index < 5; index++ {
		_, _ = generator.Observe(Signal{ResourceType: "image", ResourceID: fmt.Sprintf("i%d", index), Action: "pull", OccurredAt: base})
	}
	if generator.Pending() != 2 {
		t.Fatalf("pending = %d", generator.Pending())
	}
	clock.now = base.Add(config.CoalescingWindow + time.Second)
	out := generator.Flush(clock.now)
	stormCount := uint64(0)
	for _, event := range out {
		if event.Kind == KindEventStorm {
			stormCount += event.Count
		}
	}
	if stormCount != 3 {
		t.Fatalf("output = %+v", out)
	}
}

func TestAttributesAreDefensivelyCopied(t *testing.T) {
	at := time.Now().UTC()
	attributes := map[string]string{"name": "before"}
	event, err := Managed(Signal{ResourceType: "operation", ResourceID: "1", Action: "done", OccurredAt: at, Attributes: attributes}, "webhook:deploy")
	if err != nil {
		t.Fatal(err)
	}
	attributes["name"] = "after"
	if event.Attributes["name"] != "before" {
		t.Fatal("attributes were not copied")
	}
}

func TestDrainEmitsImmaturePendingAndStorm(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	config := DefaultConfig()
	config.Clock, config.MaxEventsPerSecond, config.MaxPending = clock, 1, 2
	generator, _ := New(config)
	for index := 0; index < 4; index++ {
		_, err := generator.Observe(Signal{
			ResourceType: "container", ResourceID: fmt.Sprintf("c%d", index),
			Action: "start", OccurredAt: base,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	out := generator.Drain(base)
	if len(out) != 2 || out[0].Kind != KindObserved || out[1].Kind != KindEventStorm || out[1].Count != 3 {
		t.Fatalf("drain = %+v", out)
	}
	if generator.Pending() != 0 || len(generator.Drain(base)) != 0 {
		t.Fatal("drain did not empty generator")
	}
}
