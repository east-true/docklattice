package auditevents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/dockeradapter"
)

type testClock struct{ now time.Time }

func (clock *testClock) Now() time.Time { return clock.now }

type fakeWAL struct {
	mu      sync.Mutex
	payload [][]byte
	err     error
	written chan struct{}
}

func (wal *fakeWAL) Append(ctx context.Context, payload []byte) (auditwal.Record, error) {
	if err := ctx.Err(); err != nil {
		return auditwal.Record{}, err
	}
	wal.mu.Lock()
	defer wal.mu.Unlock()
	if wal.err != nil {
		return auditwal.Record{}, wal.err
	}
	wal.payload = append(wal.payload, append([]byte(nil), payload...))
	record := auditwal.Record{AgentID: "agent-1", Cursor: auditwal.Cursor{Incarnation: 1, Seq: uint64(len(wal.payload))}}
	if wal.written != nil {
		select {
		case wal.written <- struct{}{}:
		default:
		}
	}
	return record, nil
}

func (wal *fakeWAL) events(t *testing.T) []auditgen.Event {
	t.Helper()
	wal.mu.Lock()
	defer wal.mu.Unlock()
	result := make([]auditgen.Event, 0, len(wal.payload))
	for _, payload := range wal.payload {
		envelope, err := Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, envelope.Event)
	}
	return result
}

type fakeSource struct {
	events chan dockeradapter.Event
	errors chan error
	ready  chan time.Time
}

func (source *fakeSource) SubscribeEvents(_ context.Context, since time.Time) (dockeradapter.EventStream, error) {
	if source.ready != nil {
		source.ready <- since
	}
	return dockeradapter.EventStream{Events: source.events, Errors: source.errors}, nil
}

type fakeInspector struct {
	mu    sync.Mutex
	calls int
	value dockeradapter.Container
}

type checkpointRecorder struct {
	mu            sync.Mutex
	clock         *testClock
	calls         []time.Time
	callWallTimes []time.Time
	called        chan struct{}
}

func (recorder *checkpointRecorder) checkpoint(_ context.Context, at time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls = append(recorder.calls, at)
	if recorder.clock != nil {
		recorder.callWallTimes = append(recorder.callWallTimes, recorder.clock.now)
	}
	if recorder.called != nil {
		select {
		case recorder.called <- struct{}{}:
		default:
		}
	}
	return nil
}

func (recorder *checkpointRecorder) snapshot() ([]time.Time, []time.Time) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]time.Time(nil), recorder.calls...), append([]time.Time(nil), recorder.callWallTimes...)
}

func (inspector *fakeInspector) Inspect(context.Context, string) (dockeradapter.Container, error) {
	inspector.mu.Lock()
	inspector.calls++
	inspector.mu.Unlock()
	return inspector.value, nil
}

func TestCodecHasStableStrictBoundedSchema(t *testing.T) {
	at := time.Date(2026, 8, 15, 1, 2, 3, 4, time.FixedZone("KST", 9*60*60))
	event, err := auditgen.Managed(auditgen.Signal{
		ResourceType: "operation", ResourceID: "op-1", Action: "completed", OccurredAt: at,
		Attributes: map[string]string{"result": "success"},
	}, "ui:127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeEnvelope(Envelope{Event: event, ProjectUID: "project-1", OperationID: "op-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"kind":"MANAGED","resource_type":"operation","resource_id":"op-1","action":"completed","actor":"ui:127.0.0.1","project_uid":"project-1","operation_id":"op-1","first_at":"2026-08-14T16:02:03.000000004Z","last_at":"2026-08-14T16:02:03.000000004Z","count":1,"attributes":{"result":"success"}}`
	if string(encoded) != want {
		t.Fatalf("payload = %s", encoded)
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded.Event.ResourceID != "op-1" || !decoded.Event.FirstAt.Equal(at) || decoded.ProjectUID != "project-1" || decoded.OperationID != "op-1" {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
	unknown := strings.Replace(string(encoded), `"version":1`, `"version":1,"unknown":true`, 1)
	if _, err := Decode([]byte(unknown)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicateTop := strings.Replace(string(encoded), `"kind":"MANAGED"`, `"kind":"MANAGED","kind":"OBSERVED"`, 1)
	if _, err := Decode([]byte(duplicateTop)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("duplicate top-level field error = %v", err)
	}
	duplicateAttribute := strings.Replace(string(encoded), `"result":"success"`, `"result":"success","result":"changed"`, 1)
	if _, err := Decode([]byte(duplicateAttribute)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("duplicate attribute error = %v", err)
	}
	if _, err := Decode(append(encoded, []byte(` {}`)...)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("trailing data error = %v", err)
	}
	if _, err := Encode(event); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("managed event without operation identity error = %v", err)
	}
	event.Attributes["too_large"] = strings.Repeat("x", maxAttributeVal+1)
	if _, err := EncodeEnvelope(Envelope{Event: event, OperationID: "op-1"}); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("oversize attribute error = %v", err)
	}
}

func TestManagedAppenderBypassesObservedGenerator(t *testing.T) {
	wal := &fakeWAL{}
	appender, _ := NewAppender(wal)
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 30; index++ {
		_, err := appender.AppendManaged(context.Background(), auditgen.Signal{
			ResourceType: "operation", ResourceID: fmt.Sprintf("op-%d", index),
			Action: "completed", OccurredAt: at,
		}, "webhook:deploy", "project-1", fmt.Sprintf("op-%d", index))
		if err != nil {
			t.Fatal(err)
		}
	}
	if events := wal.events(t); len(events) != 30 || events[29].Kind != auditgen.KindManaged {
		t.Fatalf("managed events = %d", len(events))
	}
}

func TestContinuityUncertainCodecAndAppender(t *testing.T) {
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	durable := uint64(42)
	encoded, err := EncodeContinuityUncertain(7, &durable, at)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Event.Kind != KindContinuityUncertain || envelope.PreviousIncarnation != 7 ||
		envelope.KnownDurableThrough == nil || *envelope.KnownDurableThrough != 42 ||
		envelope.Reason != ContinuityReasonUncleanShutdown || envelope.Event.Actor != "" ||
		envelope.ProjectUID != "" || envelope.OperationID != "" {
		t.Fatalf("continuity envelope = %+v", envelope)
	}
	badReason := strings.Replace(string(encoded), ContinuityReasonUncleanShutdown, "GUESSED_LOSS", 1)
	if _, err := Decode([]byte(badReason)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("bad reason error = %v", err)
	}
	wal := &fakeWAL{}
	appender, _ := NewAppender(wal)
	if _, err := appender.AppendContinuityUncertain(context.Background(), 7, nil, at); err != nil {
		t.Fatal(err)
	}
	stored := wal.events(t)
	if len(stored) != 1 || stored[0].Kind != KindContinuityUncertain {
		t.Fatalf("stored continuity events = %+v", stored)
	}
}

func TestRunnerCoalescesInspectsMeaningfulTransitionsAndBoundsAttributes(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &testClock{now: base}
	generatorConfig := auditgen.DefaultConfig()
	generatorConfig.Clock = clock
	generator, _ := auditgen.New(generatorConfig)
	wal := &fakeWAL{written: make(chan struct{}, 2)}
	appender, _ := NewAppender(wal)
	ticks := make(chan time.Time)
	source := &fakeSource{events: make(chan dockeradapter.Event), errors: make(chan error), ready: make(chan time.Time, 1)}
	inspector := &fakeInspector{value: dockeradapter.Container{State: "exited", Status: "exited", Image: "nginx", Names: []string{"/web"}}}
	runner, err := NewRunner(RunnerConfig{
		Source: source, Inspector: inspector, Generator: generator, Appender: appender,
		Ticks: ticks, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, base.Add(-time.Minute)) }()
	if got := <-source.ready; !got.Equal(base.Add(-time.Minute)) {
		t.Fatalf("since = %v", got)
	}
	attributes := map[string]string{
		"name": "web", "password": "must-not-be-retained",
		"com.docker.compose.project": "demo", "image": strings.Repeat("x", maxAttributeVal+20),
	}
	source.events <- dockeradapter.Event{ResourceType: "container", ResourceID: strings.Repeat("a", 64), Action: "die", OccurredAt: base, Attributes: attributes}
	source.events <- dockeradapter.Event{ResourceType: "container", ResourceID: strings.Repeat("a", 64), Action: "die", OccurredAt: base.Add(time.Second), Attributes: attributes}
	clock.now = base.Add(5 * time.Second)
	ticks <- clock.now
	select {
	case <-wal.written:
	case <-time.After(time.Second):
		t.Fatal("observed event was not appended")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	events := wal.events(t)
	if len(events) != 1 || events[0].Kind != auditgen.KindObserved || events[0].Count != 2 {
		t.Fatalf("events = %+v", events)
	}
	if _, exists := events[0].Attributes["password"]; exists || len(events[0].Attributes["image"]) != maxAttributeVal ||
		events[0].Attributes["inspect_state"] != "exited" {
		t.Fatalf("attributes = %+v", events[0].Attributes)
	}
	inspector.mu.Lock()
	calls := inspector.calls
	inspector.mu.Unlock()
	if calls != 2 {
		t.Fatalf("inspect calls = %d", calls)
	}
	if !runner.LastEventAt().Equal(base.Add(time.Second)) {
		t.Fatalf("last event = %v", runner.LastEventAt())
	}
}

func TestRunnerShutdownDrainProducesOneStormSummary(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &testClock{now: base}
	config := auditgen.DefaultConfig()
	config.Clock, config.MaxEventsPerSecond, config.MaxPending = clock, 1, 3
	generator, _ := auditgen.New(config)
	wal := &fakeWAL{}
	appender, _ := NewAppender(wal)
	source := &fakeSource{events: make(chan dockeradapter.Event), errors: make(chan error), ready: make(chan time.Time, 1)}
	runner, _ := NewRunner(RunnerConfig{Source: source, Generator: generator, Appender: appender, Ticks: make(chan time.Time), Now: clock.Now})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, time.Time{}) }()
	<-source.ready
	for index := 0; index < 5; index++ {
		source.events <- dockeradapter.Event{
			ResourceType: "image", ResourceID: fmt.Sprintf("image-%d", index), Action: "pull", OccurredAt: base,
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	events := wal.events(t)
	if len(events) != 2 || events[0].Kind != auditgen.KindObserved || events[1].Kind != auditgen.KindEventStorm || events[1].Count != 4 {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunnerDrainsBeforeReturningStreamError(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &testClock{now: base}
	config := auditgen.DefaultConfig()
	config.Clock = clock
	generator, _ := auditgen.New(config)
	wal := &fakeWAL{}
	appender, _ := NewAppender(wal)
	source := &fakeSource{events: make(chan dockeradapter.Event), errors: make(chan error), ready: make(chan time.Time, 1)}
	runner, _ := NewRunner(RunnerConfig{Source: source, Generator: generator, Appender: appender, Ticks: make(chan time.Time), Now: clock.Now})
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), time.Time{}) }()
	<-source.ready
	source.events <- dockeradapter.Event{ResourceType: "volume", ResourceID: "data", Action: "create", OccurredAt: base}
	wantErr := errors.New("daemon restarted")
	source.errors <- wantErr
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v", err)
	}
	if events := wal.events(t); len(events) != 1 || events[0].ResourceID != "data" {
		t.Fatalf("drained events = %+v", events)
	}
}

func TestCheckpointNeverAdvancesBeforeWALAppendSuccess(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &testClock{now: base.Add(5 * time.Second)}
	config := auditgen.DefaultConfig()
	config.Clock = clock
	generator, _ := auditgen.New(config)
	wal := &fakeWAL{err: errors.New("disk unavailable")}
	appender, _ := NewAppender(wal)
	ticks := make(chan time.Time)
	source := &fakeSource{events: make(chan dockeradapter.Event), errors: make(chan error), ready: make(chan time.Time, 1)}
	recorder := &checkpointRecorder{clock: clock}
	runner, _ := NewRunner(RunnerConfig{
		Source: source, Generator: generator, Appender: appender, Ticks: ticks,
		Now: clock.Now, Checkpoint: recorder.checkpoint,
	})
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), time.Time{}) }()
	<-source.ready
	source.events <- dockeradapter.Event{ResourceType: "container", ResourceID: "c1", Action: "start", OccurredAt: base}
	ticks <- base.Add(5 * time.Second)
	if err := <-done; err == nil {
		t.Fatal("append failure was not returned")
	}
	if calls, _ := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("checkpoint advanced before append: %v", calls)
	}
}

func TestCheckpointIsThrottledAndFinalDrainForcesNewestWatermark(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := &testClock{now: base}
	config := auditgen.DefaultConfig()
	config.Clock, config.CoalescingWindow = clock, time.Second
	generator, _ := auditgen.New(config)
	wal := &fakeWAL{written: make(chan struct{}, 3)}
	appender, _ := NewAppender(wal)
	ticks := make(chan time.Time)
	source := &fakeSource{events: make(chan dockeradapter.Event), errors: make(chan error), ready: make(chan time.Time, 1)}
	recorder := &checkpointRecorder{clock: clock, called: make(chan struct{}, 2)}
	runner, _ := NewRunner(RunnerConfig{
		Source: source, Generator: generator, Appender: appender, Ticks: ticks,
		Now: clock.Now, Checkpoint: recorder.checkpoint,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, time.Time{}) }()
	<-source.ready
	for index, offset := range []time.Duration{0, 2 * time.Second, 4 * time.Second} {
		source.events <- dockeradapter.Event{
			ResourceType: "container", ResourceID: fmt.Sprintf("c%d", index), Action: "start", OccurredAt: base.Add(offset),
		}
		if index == 1 {
			select {
			case <-recorder.called:
			case <-time.After(time.Second):
				t.Fatal("first checkpoint was not called")
			}
			<-wal.written
			clock.now = base.Add(500 * time.Millisecond)
		}
	}
	select {
	case <-wal.written:
	case <-time.After(time.Second):
		t.Fatal("second event was not appended")
	}
	// The third signal emitted the second record inside the same wall-clock
	// second; its checkpoint remains coalesced. Cancellation drains the third
	// record and forces the final newest watermark despite the throttle.
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls, wallTimes := recorder.snapshot()
	if len(calls) != 2 || !calls[0].Equal(base) || !calls[1].Equal(base.Add(4*time.Second)) {
		t.Fatalf("checkpoint calls = %v", calls)
	}
	if len(wallTimes) != 2 || wallTimes[1].Sub(wallTimes[0]) >= time.Second {
		// The second call is intentionally the final-drain exception.
		t.Fatalf("checkpoint wall times = %v", wallTimes)
	}
}
