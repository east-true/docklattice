package auditevents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/auditgen"
	"github.com/east-true/docklattice/internal/dockeradapter"
)

var (
	ErrInvalidConfig  = errors.New("AUDIT_EVENTS_INVALID_CONFIG")
	ErrStreamClosed   = errors.New("DOCKER_EVENT_STREAM_CLOSED")
	ErrAlreadyRunning = errors.New("AUDIT_EVENTS_ALREADY_RUNNING")
)

type Source interface {
	SubscribeEvents(context.Context, time.Time) (dockeradapter.EventStream, error)
}

type Inspector interface {
	Inspect(context.Context, string) (dockeradapter.Container, error)
}

type RunnerConfig struct {
	Source               Source
	Inspector            Inspector
	Generator            *auditgen.Generator
	Appender             *Appender
	FlushInterval        time.Duration
	ShutdownFlushTimeout time.Duration
	Now                  func() time.Time
	// Checkpoint durably stores a proven Docker event watermark. It is called
	// only after all audit records represented by that watermark reached WAL.
	Checkpoint func(context.Context, time.Time) error
	// Ticks is a deterministic test seam. Production leaves it nil and uses
	// FlushInterval; callers must not close the supplied channel while Run is active.
	Ticks <-chan time.Time
}

type Runner struct {
	config              RunnerConfig
	mu                  sync.RWMutex
	lastAt              time.Time
	running             bool
	checkpointCandidate time.Time
	checkpointedAt      time.Time
	lastCheckpointCall  time.Time
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Source == nil || config.Generator == nil || config.Appender == nil {
		return nil, ErrInvalidConfig
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = time.Second
	}
	if config.ShutdownFlushTimeout <= 0 {
		config.ShutdownFlushTimeout = 5 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runner{config: config}, nil
}

// Run consumes one Docker event subscription. A terminal stream error is
// returned after pending records are drained so the Agent runtime can perform
// the §11.2 snapshot/reconnect sequence with LastEventAt as its --since value.
func (runner *Runner) Run(ctx context.Context, since time.Time) error {
	runner.mu.Lock()
	if runner.running {
		runner.mu.Unlock()
		return ErrAlreadyRunning
	}
	runner.running = true
	runner.mu.Unlock()
	defer func() {
		runner.mu.Lock()
		runner.running = false
		runner.mu.Unlock()
	}()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stream, err := runner.config.Source.SubscribeEvents(streamCtx, since)
	if err != nil {
		return fmt.Errorf("subscribe Docker events: %w", err)
	}
	ticks := runner.config.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(runner.config.FlushInterval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	eventsIn, errorsIn := stream.Events, stream.Errors
	for {
		if eventsIn == nil && errorsIn == nil {
			if err := runner.drain(ctx, true); err != nil {
				return err
			}
			return ErrStreamClosed
		}
		select {
		case <-ctx.Done():
			cancelStream()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runner.config.ShutdownFlushTimeout)
			err := runner.drain(shutdownCtx, true)
			cancel()
			return err
		case at, ok := <-ticks:
			if !ok {
				ticks = nil
				continue
			}
			watermark, err := runner.appendAll(ctx, runner.config.Generator.Flush(at))
			if err != nil {
				return err
			}
			if err := runner.checkpoint(ctx, watermark, false); err != nil {
				return err
			}
		case event, ok := <-eventsIn:
			if !ok {
				eventsIn = nil
				continue
			}
			if err := runner.observe(ctx, event); err != nil {
				return err
			}
		case streamErr, ok := <-errorsIn:
			if !ok {
				errorsIn = nil
				continue
			}
			if err := runner.drain(ctx, true); err != nil {
				return err
			}
			if streamErr == nil || errors.Is(streamErr, io.EOF) {
				return ErrStreamClosed
			}
			return fmt.Errorf("Docker event stream: %w", streamErr)
		}
	}
}

func (runner *Runner) LastEventAt() time.Time {
	runner.mu.RLock()
	defer runner.mu.RUnlock()
	return runner.lastAt
}

func (runner *Runner) observe(ctx context.Context, event dockeradapter.Event) error {
	at := event.OccurredAt.UTC()
	if at.IsZero() {
		at = runner.config.Now().UTC()
	}
	runner.recordLastAt(at)
	attributes := selectedAttributes(event.Attributes)
	if runner.config.Inspector != nil && event.ResourceType == "container" && meaningfulTransition(event.Action) {
		if current, err := runner.config.Inspector.Inspect(ctx, event.ResourceID); err == nil {
			attributes = mergeInspectAttributes(attributes, current)
		}
	}
	emitted, err := runner.config.Generator.Observe(auditgen.Signal{
		ResourceType: event.ResourceType, ResourceID: event.ResourceID,
		Action: event.Action, OccurredAt: at, Attributes: attributes,
	})
	if err != nil {
		return fmt.Errorf("generate observed audit: %w", err)
	}
	watermark, err := runner.appendAll(ctx, emitted)
	if err != nil {
		return err
	}
	return runner.checkpoint(ctx, watermark, false)
}

func (runner *Runner) appendAll(ctx context.Context, events []auditgen.Event) (time.Time, error) {
	var watermark time.Time
	for _, event := range events {
		if _, err := runner.config.Appender.Append(ctx, event); err != nil {
			return time.Time{}, err
		}
		if event.LastAt.After(watermark) {
			watermark = event.LastAt.UTC()
		}
	}
	return watermark, nil
}

func (runner *Runner) drain(ctx context.Context, final bool) error {
	watermark, err := runner.appendAll(ctx, runner.config.Generator.Drain(runner.config.Now().UTC()))
	if err != nil {
		return err
	}
	if final {
		// Drain has now represented every accepted pending event. Advancing over
		// later ignored Docker actions is safe because they intentionally emit no audit.
		if last := runner.LastEventAt(); last.After(watermark) {
			watermark = last
		}
	}
	return runner.checkpoint(ctx, watermark, final)
}

func (runner *Runner) checkpoint(ctx context.Context, watermark time.Time, force bool) error {
	if runner.config.Checkpoint == nil {
		return nil
	}
	watermark = watermark.UTC()
	if watermark.After(runner.checkpointCandidate) {
		runner.checkpointCandidate = watermark
	}
	if !runner.checkpointCandidate.After(runner.checkpointedAt) {
		return nil
	}
	now := runner.config.Now().UTC()
	if !force && !runner.lastCheckpointCall.IsZero() && now.Sub(runner.lastCheckpointCall) < time.Second {
		return nil
	}
	candidate := runner.checkpointCandidate
	runner.lastCheckpointCall = now
	if err := runner.config.Checkpoint(ctx, candidate); err != nil {
		return fmt.Errorf("persist Docker event checkpoint: %w", err)
	}
	runner.checkpointedAt = candidate
	return nil
}

func (runner *Runner) recordLastAt(at time.Time) {
	runner.mu.Lock()
	if at.After(runner.lastAt) {
		runner.lastAt = at
	}
	runner.mu.Unlock()
}

func meaningfulTransition(action string) bool {
	return action == "die" || action == "health_status" || strings.HasPrefix(action, "health_status:")
}

var retainedDockerAttributes = map[string]struct{}{
	"name": {}, "image": {}, "exitCode": {}, "signal": {},
	"com.docker.compose.project": {}, "com.docker.compose.service": {},
	"com.docker.compose.project.working_dir": {}, "com.docker.compose.project.config_files": {},
}

func selectedAttributes(input map[string]string) map[string]string {
	keys := make([]string, 0, len(input))
	for key := range input {
		if _, retained := retainedDockerAttributes[key]; retained {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	output := make(map[string]string, len(keys))
	for _, key := range keys {
		output[key] = truncateUTF8(input[key], maxAttributeVal)
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func mergeInspectAttributes(attributes map[string]string, current dockeradapter.Container) map[string]string {
	if attributes == nil {
		attributes = make(map[string]string)
	}
	for key, value := range map[string]string{
		"inspect_state": current.State, "inspect_status": current.Status, "inspect_image": current.Image,
		"inspect_health": current.Health,
	} {
		if value != "" {
			attributes[key] = truncateUTF8(value, maxAttributeVal)
		}
	}
	if len(current.Names) > 0 && current.Names[0] != "" {
		attributes["inspect_name"] = truncateUTF8(current.Names[0], maxAttributeVal)
	}
	if current.State == "exited" || current.State == "dead" {
		attributes["inspect_exit_code"] = fmt.Sprint(current.ExitCode)
	}
	return attributes
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
