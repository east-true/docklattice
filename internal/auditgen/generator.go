// Package auditgen turns managed Operations and whitelisted Docker event
// signals into bounded Audit records. Persistence and cursor identity belong
// to auditwal, not this package.
package auditgen

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	productconfig "github.com/east-true/docklattice/internal/config"
)

var ErrInvalidAudit = errors.New("invalid audit input")

type Kind string

const (
	KindManaged    Kind = "MANAGED"
	KindObserved   Kind = "OBSERVED"
	KindEventStorm Kind = "EVENT_STORM"
)

type Signal struct {
	ResourceType string
	ResourceID   string
	Action       string
	OccurredAt   time.Time
	Attributes   map[string]string
}

type Event struct {
	Kind         Kind
	ResourceType string
	ResourceID   string
	Action       string
	Actor        string
	FirstAt      time.Time
	LastAt       time.Time
	Count        uint64
	Attributes   map[string]string
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Config struct {
	Clock              Clock
	CoalescingWindow   time.Duration
	MaxEventsPerSecond int
	MaxPending         int
}

func DefaultConfig() Config {
	defaults := productconfig.V1Defaults()
	maxPending := defaults.ObservedAuditMaxPerSecond * int(defaults.EventCoalescingWindow/time.Second)
	if maxPending < defaults.ObservedAuditMaxPerSecond {
		maxPending = defaults.ObservedAuditMaxPerSecond
	}
	return Config{
		Clock: realClock{}, CoalescingWindow: defaults.EventCoalescingWindow,
		MaxEventsPerSecond: defaults.ObservedAuditMaxPerSecond, MaxPending: maxPending,
	}
}

type pendingEvent struct{ event Event }

type Generator struct {
	config  Config
	pending map[string]pendingEvent
	bucket  time.Time
	emitted int
	storm   *Event
}

func New(config Config) (*Generator, error) {
	if config.Clock == nil || config.CoalescingWindow <= 0 || config.MaxEventsPerSecond <= 0 || config.MaxPending <= 0 {
		return nil, fmt.Errorf("%w: audit generator bounds must be positive", ErrInvalidAudit)
	}
	return &Generator{config: config, pending: make(map[string]pendingEvent)}, nil
}

// Managed emits exactly one record per operation lifecycle completion. It has
// no coalescing or rate limit.
func Managed(signal Signal, actor string) (Event, error) {
	if err := validateSignal(signal); err != nil {
		return Event{}, err
	}
	if err := validateActor(actor); err != nil {
		return Event{}, err
	}
	at := signal.OccurredAt.UTC()
	if at.IsZero() {
		return Event{}, fmt.Errorf("%w: managed event requires occurred_at", ErrInvalidAudit)
	}
	return Event{
		Kind: KindManaged, ResourceType: signal.ResourceType, ResourceID: signal.ResourceID,
		Action: signal.Action, Actor: actor, FirstAt: at, LastAt: at, Count: 1,
		Attributes: cloneAttributes(signal.Attributes),
	}, nil
}

// Observe accepts only the architecture's Docker Events whitelist. Repeated
// keys are held for the fixed coalescing window; callers periodically call
// Flush even when no new signal arrives.
func (generator *Generator) Observe(signal Signal) ([]Event, error) {
	if !whitelisted(signal.ResourceType, signal.Action) {
		return generator.Flush(generator.now()), nil
	}
	if err := validateSignal(signal); err != nil {
		return nil, err
	}
	now := signal.OccurredAt.UTC()
	if now.IsZero() {
		now = generator.now()
	}
	emitted := generator.Flush(now)
	key := signal.ResourceType + "\x00" + signal.ResourceID + "\x00" + normalizedAction(signal.Action)
	if current, exists := generator.pending[key]; exists {
		current.event.LastAt = now
		current.event.Count++
		generator.pending[key] = current
		return emitted, nil
	}
	if len(generator.pending) >= generator.config.MaxPending {
		generator.addStorm(now, 1)
		return emitted, nil
	}
	generator.pending[key] = pendingEvent{event: Event{
		Kind: KindObserved, ResourceType: signal.ResourceType, ResourceID: signal.ResourceID,
		Action: normalizedAction(signal.Action), Actor: "", FirstAt: now, LastAt: now, Count: 1,
		Attributes: cloneAttributes(signal.Attributes),
	}}
	return emitted, nil
}

func (generator *Generator) Flush(now time.Time) []Event {
	now = now.UTC()
	generator.rotateBucket(now)
	keys := make([]string, 0, len(generator.pending))
	for key, pending := range generator.pending {
		if !now.Before(pending.event.FirstAt.Add(generator.config.CoalescingWindow)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make([]Event, 0, len(keys)+1)
	for _, key := range keys {
		event := generator.pending[key].event
		delete(generator.pending, key)
		if generator.emitted < generator.config.MaxEventsPerSecond {
			generator.emitted++
			result = append(result, cloneEvent(event))
		} else {
			generator.addStorm(event.LastAt, event.Count)
		}
	}
	return append(result, generator.takeFinishedStorm(now)...)
}

// Drain emits every pending coalesced event and any event-storm summary. It is
// intended for the Agent's graceful shutdown sequence after the Docker event
// subscription has stopped. Rate limiting still applies; the summary itself
// is never left pending.
func (generator *Generator) Drain(now time.Time) []Event {
	now = now.UTC()
	generator.rotateBucket(now)
	keys := make([]string, 0, len(generator.pending))
	for key := range generator.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Event, 0, len(keys)+1)
	for _, key := range keys {
		event := generator.pending[key].event
		delete(generator.pending, key)
		if generator.emitted < generator.config.MaxEventsPerSecond {
			generator.emitted++
			result = append(result, cloneEvent(event))
		} else {
			generator.addStorm(event.LastAt, event.Count)
		}
	}
	if generator.storm != nil {
		result = append(result, cloneEvent(*generator.storm))
		generator.storm = nil
	}
	return result
}

func (generator *Generator) Pending() int { return len(generator.pending) }

func (generator *Generator) now() time.Time { return generator.config.Clock.Now().UTC() }

func (generator *Generator) rotateBucket(now time.Time) {
	bucket := now.Truncate(time.Second)
	if generator.bucket.IsZero() {
		generator.bucket = bucket
		return
	}
	if bucket.After(generator.bucket) {
		generator.bucket = bucket
		generator.emitted = 0
	}
}

func (generator *Generator) addStorm(at time.Time, count uint64) {
	if generator.storm == nil {
		generator.storm = &Event{Kind: KindEventStorm, ResourceType: "docker", Action: "event_storm", FirstAt: at, LastAt: at}
	}
	generator.storm.LastAt = at
	generator.storm.Count += count
}

func (generator *Generator) takeFinishedStorm(now time.Time) []Event {
	if generator.storm == nil || !now.Truncate(time.Second).After(generator.storm.LastAt.Truncate(time.Second)) {
		return nil
	}
	event := cloneEvent(*generator.storm)
	generator.storm = nil
	return []Event{event}
}

func whitelisted(resourceType, action string) bool {
	action = normalizedAction(action)
	switch resourceType {
	case "container":
		switch action {
		case "create", "start", "die", "stop", "kill", "destroy", "health_status", "rename":
			return true
		}
	case "image":
		return action == "pull" || action == "delete" || action == "tag"
	case "volume", "network":
		return action == "create" || action == "destroy"
	}
	return false
}

func normalizedAction(action string) string {
	if strings.HasPrefix(action, "health_status:") {
		return "health_status"
	}
	return action
}

func validateSignal(signal Signal) error {
	if signal.ResourceType == "" || signal.ResourceID == "" || signal.Action == "" ||
		strings.ContainsRune(signal.ResourceType, 0) || strings.ContainsRune(signal.ResourceID, 0) || strings.ContainsRune(signal.Action, 0) {
		return ErrInvalidAudit
	}
	return nil
}

func validateActor(actor string) error {
	if actor == "" {
		return nil
	}
	if strings.HasPrefix(actor, "ui:") {
		if net.ParseIP(strings.TrimPrefix(actor, "ui:")) == nil {
			return fmt.Errorf("%w: invalid UI client IP", ErrInvalidAudit)
		}
		return nil
	}
	if strings.HasPrefix(actor, "webhook:") {
		provider := strings.TrimPrefix(actor, "webhook:")
		if provider != "" && len(provider) <= 128 && !strings.ContainsAny(provider, "\x00\r\n") {
			return nil
		}
	}
	return fmt.Errorf("%w: actor is not representable", ErrInvalidAudit)
}

func cloneEvent(event Event) Event {
	event.Attributes = cloneAttributes(event.Attributes)
	return event
}

func cloneAttributes(attributes map[string]string) map[string]string {
	if attributes == nil {
		return nil
	}
	copy := make(map[string]string, len(attributes))
	for key, value := range attributes {
		copy[key] = value
	}
	return copy
}
