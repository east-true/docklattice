// Package logrelay provides bounded, non-persistent live log forwarding. It
// intentionally has no resume cursor or storage API: browser disconnect ends
// the Docker stream and reconnect starts a new stream explicitly.
package logrelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	DefaultBytesPerSecond    int64 = 1 << 20
	DefaultMaxBufferedBytes        = 1 << 20
	DefaultMaxBufferedChunks       = 256
)

type Request struct {
	ContainerID string
	ProjectUID  string
	Services    []string
	Follow      bool
	TailLines   uint64
	ShowStdout  bool
	ShowStderr  bool
	Timestamps  bool
	Since       string
	Until       string
}

type StreamKind string

const (
	Stdout StreamKind = "STDOUT"
	Stderr StreamKind = "STDERR"
)

type Chunk struct {
	Data   []byte
	Stream StreamKind
	// DroppedBytes/DroppedLines report loss before this relay, such as the
	// non-blocking Compose child-process drain. Relay-local drops are added to
	// the same explicit stream accounting.
	DroppedBytes uint64
	DroppedLines uint64
	// LineCount should contain the source's exact logical line count. If zero,
	// the relay conservatively counts newline delimiters and relies on the
	// always-exact dropped byte count for partial lines.
	LineCount uint64
	Timestamp time.Time
}

// Source is the Docker-adapter integration boundary. Implementations must
// stop reading Docker logs when ctx is canceled and must not auto-resume.
type Source interface {
	Stream(context.Context, Request, func(Chunk) error) error
}

type SourceFunc func(context.Context, Request, func(Chunk) error) error

func (f SourceFunc) Stream(ctx context.Context, request Request, emit func(Chunk) error) error {
	return f(ctx, request, emit)
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Config struct {
	Source            Source
	BytesPerSecond    int64
	MaxBufferedBytes  int
	MaxBufferedChunks int
	Clock             Clock
}

type Relay struct{ config Config }

func New(config Config) (*Relay, error) {
	if config.Source == nil {
		return nil, errors.New("log source is required")
	}
	if config.BytesPerSecond == 0 {
		config.BytesPerSecond = DefaultBytesPerSecond
	}
	if config.MaxBufferedBytes == 0 {
		config.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if config.MaxBufferedChunks == 0 {
		config.MaxBufferedChunks = DefaultMaxBufferedChunks
	}
	if config.BytesPerSecond <= 0 || config.MaxBufferedBytes <= 0 || config.MaxBufferedChunks <= 0 {
		return nil, errors.New("log rate and buffer limits must be positive")
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	return &Relay{config: config}, nil
}

func (r *Relay) Open(ctx context.Context, request Request) (*Stream, error) {
	if ctx == nil {
		return nil, errors.New("log stream context is required")
	}
	if request.ProjectUID != "" {
		if len(request.Services) > 256 || request.ContainerID != "" && len(request.Services) != 0 {
			return nil, errors.New("project logs require a bounded project target and either services or one Container")
		}
	} else if request.ContainerID == "" || len(request.Services) != 0 {
		return nil, errors.New("container logs require only a container ID")
	}
	if err := validateLogWindow(request.Since, request.Until); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !request.ShowStdout && !request.ShowStderr {
		request.ShowStdout, request.ShowStderr = true, true
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream := &Stream{
		ctx: streamCtx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
		maxBytes: r.config.MaxBufferedBytes, maxChunks: r.config.MaxBufferedChunks,
		limiter: newByteLimiter(r.config.BytesPerSecond, r.config.Clock),
	}
	go stream.run(r.config.Source, request)
	return stream, nil
}

func validateLogWindow(sinceValue, untilValue string) error {
	var since, until time.Time
	var err error
	if sinceValue != "" {
		if len(sinceValue) > 64 {
			return errors.New("log since time is invalid")
		}
		since, err = time.Parse(time.RFC3339Nano, sinceValue)
		if err != nil {
			return errors.New("log since time is invalid")
		}
	}
	if untilValue != "" {
		if len(untilValue) > 64 {
			return errors.New("log until time is invalid")
		}
		until, err = time.Parse(time.RFC3339Nano, untilValue)
		if err != nil {
			return errors.New("log until time is invalid")
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return errors.New("log since time must not be after until time")
	}
	return nil
}

type queuedChunk struct {
	chunk Chunk
	lines uint64
}

type Event struct {
	Data         []byte
	Stream       StreamKind
	LineCount    uint64
	Timestamp    time.Time
	DroppedBytes uint64
	DroppedLines uint64
	Terminal     bool
	Error        string
}

type Stream struct {
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
	nextMu sync.Mutex

	mu               sync.Mutex
	queue            []queuedChunk
	bufferedBytes    int
	maxBytes         int
	maxChunks        int
	droppedBytes     uint64
	droppedLines     uint64
	sourceDone       bool
	sourceErr        error
	terminalReported bool
	limiter          *byteLimiter
}

func (s *Stream) run(source Source, request Request) {
	err := source.Stream(s.ctx, request, s.enqueue)
	s.mu.Lock()
	s.sourceDone = true
	if err != nil && !errors.Is(err, context.Canceled) {
		s.sourceErr = err
	} else if s.ctx.Err() != nil {
		s.sourceErr = s.ctx.Err()
	}
	s.mu.Unlock()
	s.signal()
	close(s.done)
}

func (s *Stream) enqueue(chunk Chunk) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if chunk.DroppedBytes > 0 || chunk.DroppedLines > 0 {
		s.mu.Lock()
		s.addReportedDrop(chunk.DroppedBytes, chunk.DroppedLines)
		s.mu.Unlock()
		s.signal()
	}
	if len(chunk.Data) == 0 {
		return nil
	}
	lines := chunk.LineCount
	if lines == 0 {
		lines = countLines(chunk.Data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.limiter.Allow(len(chunk.Data)) {
		s.addDrop(len(chunk.Data), lines)
		s.signal()
		return nil
	}
	if len(chunk.Data) > s.maxBytes {
		s.addDrop(len(chunk.Data), lines)
		s.signal()
		return nil
	}
	for len(s.queue) > 0 && (s.bufferedBytes+len(chunk.Data) > s.maxBytes || len(s.queue) >= s.maxChunks) {
		s.dropOldest()
	}
	copyChunk := chunk
	copyChunk.Data = append([]byte(nil), chunk.Data...)
	s.queue = append(s.queue, queuedChunk{chunk: copyChunk, lines: lines})
	s.bufferedBytes += len(copyChunk.Data)
	s.signal()
	return nil
}

func (s *Stream) dropOldest() {
	oldest := s.queue[0]
	s.queue[0] = queuedChunk{}
	s.queue = s.queue[1:]
	s.bufferedBytes -= len(oldest.chunk.Data)
	s.addDrop(len(oldest.chunk.Data), oldest.lines)
}

func (s *Stream) addDrop(bytes int, lines uint64) {
	s.addReportedDrop(uint64(bytes), lines)
}

func (s *Stream) addReportedDrop(bytes, lines uint64) {
	s.droppedBytes += bytes
	s.droppedLines += lines
}

func (s *Stream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Next returns queued data and any drops which occurred before its delivery.
// Only one goroutine may call Next; this is enforced so ordering stays exact.
func (s *Stream) Next(ctx context.Context) (Event, error) {
	if ctx == nil {
		return Event{}, errors.New("Next context is required")
	}
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			item := s.queue[0]
			s.queue[0] = queuedChunk{}
			s.queue = s.queue[1:]
			s.bufferedBytes -= len(item.chunk.Data)
			event := Event{
				Data: item.chunk.Data, Stream: item.chunk.Stream, LineCount: item.lines, Timestamp: item.chunk.Timestamp,
				DroppedBytes: s.droppedBytes, DroppedLines: s.droppedLines,
			}
			s.droppedBytes, s.droppedLines = 0, 0
			s.mu.Unlock()
			return event, nil
		}
		if s.sourceDone && !s.terminalReported {
			s.terminalReported = true
			event := Event{DroppedBytes: s.droppedBytes, DroppedLines: s.droppedLines, Terminal: true}
			s.droppedBytes, s.droppedLines = 0, 0
			if s.sourceErr != nil {
				event.Error = s.sourceErr.Error()
			}
			s.mu.Unlock()
			return event, nil
		}
		if s.sourceDone {
			s.mu.Unlock()
			return Event{}, io.EOF
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-s.wake:
		}
	}
}

func (s *Stream) Buffered() (bytes, chunks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bufferedBytes, len(s.queue)
}

func (s *Stream) Done() <-chan struct{} { return s.done }

func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sourceErr
}

func (s *Stream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

type byteLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	clock  Clock
}

func newByteLimiter(rate int64, clock Clock) *byteLimiter {
	now := clock.Now()
	return &byteLimiter{rate: float64(rate), tokens: float64(rate), last: now, clock: clock}
}

func (l *byteLimiter) Allow(bytes int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if now.After(l.last) {
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.rate {
			l.tokens = l.rate
		}
		l.last = now
	}
	if float64(bytes) > l.tokens {
		return false
	}
	l.tokens -= float64(bytes)
	return true
}

func countLines(data []byte) uint64 {
	return uint64(bytes.Count(data, []byte{'\n'}))
}

func (e Event) Validate() error {
	if e.Terminal && len(e.Data) > 0 {
		return fmt.Errorf("terminal log event cannot contain data")
	}
	return nil
}
