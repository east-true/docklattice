package dockeradapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

type eventEngine struct {
	*fakeEngine
	options client.EventsListOptions
	result  client.EventsResult
}

func (engine *eventEngine) Events(_ context.Context, options client.EventsListOptions) client.EventsResult {
	engine.options = options
	return engine.result
}

func TestSubscribeEventsFiltersTypesAndForwardsWithoutBuffering(t *testing.T) {
	messages := make(chan events.Message)
	errorsIn := make(chan error, 1)
	engine := &eventEngine{fakeEngine: &fakeEngine{}, result: client.EventsResult{Messages: messages, Err: errorsIn}}
	adapter, err := New(engine, func() agentsafety.Identification { return agentsafety.Identification{} }, MinimumAPIVersion)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 8, 15, 1, 2, 3, 4, time.FixedZone("test", 9*60*60))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := adapter.SubscribeEvents(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if engine.options.Since != since.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("since = %q", engine.options.Since)
	}
	for _, kind := range []string{"container", "image", "volume", "network"} {
		if !engine.options.Filters["type"][kind] {
			t.Fatalf("missing type filter %q: %+v", kind, engine.options.Filters)
		}
	}
	attributes := map[string]string{"name": "web"}
	messageAt := time.Date(2026, 8, 15, 0, 0, 0, 123, time.UTC)
	go func() {
		messages <- events.Message{
			Type: events.ContainerEventType, Action: events.ActionStart,
			Actor: events.Actor{ID: "container-1", Attributes: attributes}, TimeNano: messageAt.UnixNano(),
		}
	}()
	event := <-stream.Events
	attributes["name"] = "changed"
	if event.ResourceType != "container" || event.Action != "start" || event.ResourceID != "container-1" ||
		!event.OccurredAt.Equal(messageAt) || event.Attributes["name"] != "web" {
		t.Fatalf("event = %+v", event)
	}
	wantErr := errors.New("stream failed")
	errorsIn <- wantErr
	if got := <-stream.Errors; !errors.Is(got, wantErr) {
		t.Fatalf("error = %v", got)
	}
}

func TestSubscribeEventsCancellationClosesForwarder(t *testing.T) {
	messages := make(chan events.Message)
	errorsIn := make(chan error)
	engine := &eventEngine{fakeEngine: &fakeEngine{}, result: client.EventsResult{Messages: messages, Err: errorsIn}}
	adapter, err := New(engine, func() agentsafety.Identification { return agentsafety.Identification{} }, MinimumAPIVersion)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := adapter.SubscribeEvents(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-stream.Events:
		if ok {
			t.Fatal("event channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("event forwarder did not stop")
	}
}
