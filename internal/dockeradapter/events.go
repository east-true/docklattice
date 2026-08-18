package dockeradapter

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// Event is the bounded package-neutral portion of a Docker event needed by
// the audit pipeline. Consumers must still bound and select Attributes before
// persistence because Docker labels are controlled by workloads.
type Event struct {
	ResourceType string
	ResourceID   string
	Action       string
	OccurredAt   time.Time
	Attributes   map[string]string
}

// EventStream remains valid until its context is cancelled or Errors reports
// a terminal result. Both channels are bounded by backpressure; the adapter
// does not accumulate an event queue.
type EventStream struct {
	Events <-chan Event
	Errors <-chan error
}

type eventsEngine interface {
	Events(context.Context, client.EventsListOptions) client.EventsResult
}

// SubscribeEvents subscribes to only resource types in the v1 observed-audit
// scope. The audit generator remains the authoritative action whitelist.
func (adapter *Adapter) SubscribeEvents(ctx context.Context, since time.Time) (EventStream, error) {
	api, ok := adapter.engine.(eventsEngine)
	if !ok {
		return EventStream{}, fmt.Errorf("%w: Engine client does not support events", ErrUnavailable)
	}
	filters := make(client.Filters).Add("type", "container", "image", "volume", "network")
	options := client.EventsListOptions{Filters: filters}
	if !since.IsZero() {
		options.Since = since.UTC().Format(time.RFC3339Nano)
	}
	result := api.Events(ctx, options)
	eventsOut := make(chan Event)
	errorsOut := make(chan error, 1)
	go forwardEvents(ctx, result, eventsOut, errorsOut)
	return EventStream{Events: eventsOut, Errors: errorsOut}, nil
}

func forwardEvents(ctx context.Context, source client.EventsResult, output chan<- Event, errorsOut chan<- error) {
	defer close(output)
	defer close(errorsOut)
	messages := source.Messages
	errorsIn := source.Err
	for messages != nil || errorsIn != nil {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			event := fromDockerEvent(message)
			select {
			case output <- event:
			case <-ctx.Done():
				return
			}
		case err, ok := <-errorsIn:
			if !ok {
				return
			}
			if err != nil {
				select {
				case errorsOut <- err:
				case <-ctx.Done():
				}
			}
			return
		}
	}
}

func fromDockerEvent(message events.Message) Event {
	occurredAt := time.Time{}
	if message.TimeNano > 0 {
		occurredAt = time.Unix(0, message.TimeNano).UTC()
	} else if message.Time > 0 {
		occurredAt = time.Unix(message.Time, 0).UTC()
	}
	attributes := make(map[string]string, len(message.Actor.Attributes))
	for key, value := range message.Actor.Attributes {
		attributes[key] = value
	}
	if len(attributes) == 0 {
		attributes = nil
	}
	return Event{
		ResourceType: string(message.Type), ResourceID: message.Actor.ID,
		Action: string(message.Action), OccurredAt: occurredAt, Attributes: attributes,
	}
}
