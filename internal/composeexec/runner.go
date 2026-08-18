package composeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultCancelGrace = 10 * time.Second

var ErrUnsupportedPlatform = errors.New("compose execution requires Linux process groups")

type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// OutputChunk is delivered best-effort. A DroppedBytes marker explicitly
// reports bytes omitted while the bounded consumer channel was full.
type OutputChunk struct {
	Stream       Stream
	Data         []byte
	DroppedBytes uint64
}

type Runner struct {
	DockerPath  string
	CancelGrace time.Duration
	TailBytes   int
	Env         []string
}

type Result struct {
	Args              []string
	ExitCode          int
	Tail              []byte
	TailTruncated     bool
	RelayDroppedBytes uint64
	Canceled          bool
	TimedOut          bool
	EscalatedToKill   bool
}

func (result Result) Success() bool { return !result.Canceled && result.ExitCode == 0 }

// Run starts a dedicated process group with exec.Command (never a shell and
// never exec.CommandContext). Context cancellation and deadlines share the
// SIGTERM -> grace -> SIGKILL process-group path.
func (runner Runner) Run(ctx context.Context, spec Spec, relay chan<- OutputChunk) (Result, error) {
	args, err := BuildArgs(spec)
	if err != nil {
		return Result{}, err
	}
	if err := platformSupported(); err != nil {
		return Result{}, err
	}
	dockerPath := runner.DockerPath
	if dockerPath == "" {
		dockerPath = "docker"
	}
	grace := runner.CancelGrace
	if grace <= 0 {
		grace = DefaultCancelGrace
	}
	tailBytes := runner.TailBytes
	if spec.OutputTailBytes > 0 {
		tailBytes = spec.OutputTailBytes
	}
	tail := NewTailWriter(tailBytes)
	relayState := &nonBlockingRelay{channel: relay}
	cmd := exec.Command(dockerPath, args...)
	if runner.Env != nil {
		cmd.Env = append([]string(nil), runner.Env...)
	}
	configureProcessGroup(cmd)
	cmd.Stdout = io.MultiWriter(tail, relayState.writer(StreamStdout))
	cmd.Stderr = io.MultiWriter(tail, relayState.writer(StreamStderr))
	result := Result{Args: append([]string(nil), args...), ExitCode: -1}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("composeexec: start docker compose: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		// Prefer a naturally completed process if completion raced cancellation.
		select {
		case waitErr = <-waited:
		default:
			result.Canceled = true
			result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			if err := terminateProcessGroup(cmd.Process.Pid); err != nil && !isProcessDone(err) {
				return finalizeResult(result, cmd, tail, relayState), fmt.Errorf("composeexec: SIGTERM process group: %w", err)
			}
			timer := time.NewTimer(grace)
			select {
			case waitErr = <-waited:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				result.EscalatedToKill = true
				if err := killProcessGroup(cmd.Process.Pid); err != nil && !isProcessDone(err) {
					return finalizeResult(result, cmd, tail, relayState), fmt.Errorf("composeexec: SIGKILL process group: %w", err)
				}
				waitErr = <-waited
			}
		}
	}

	result = finalizeResult(result, cmd, tail, relayState)
	var exitError *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitError) {
		return result, fmt.Errorf("composeexec: wait: %w", waitErr)
	}
	return result, nil
}

func finalizeResult(result Result, cmd *exec.Cmd, tail *TailWriter, relay *nonBlockingRelay) Result {
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	relay.flushMarker()
	result.Tail = tail.Bytes()
	result.TailTruncated = tail.Truncated()
	result.RelayDroppedBytes = relay.totalDropped.Load()
	return result
}

type nonBlockingRelay struct {
	channel      chan<- OutputChunk
	mu           sync.Mutex
	pendingDrop  uint64
	totalDropped atomic.Uint64
}

type streamRelayWriter struct {
	parent *nonBlockingRelay
	stream Stream
}

func (relay *nonBlockingRelay) writer(stream Stream) io.Writer {
	return streamRelayWriter{parent: relay, stream: stream}
}

func (writer streamRelayWriter) Write(payload []byte) (int, error) {
	writer.parent.send(writer.stream, payload)
	return len(payload), nil
}

func (relay *nonBlockingRelay) send(stream Stream, payload []byte) {
	if relay.channel == nil || len(payload) == 0 {
		return
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.pendingDrop > 0 {
		marker := OutputChunk{Stream: stream, DroppedBytes: relay.pendingDrop}
		select {
		case relay.channel <- marker:
			relay.pendingDrop = 0
		default:
			relay.pendingDrop += uint64(len(payload))
			relay.totalDropped.Add(uint64(len(payload)))
			return
		}
	}
	copyPayload := append([]byte(nil), payload...)
	select {
	case relay.channel <- OutputChunk{Stream: stream, Data: copyPayload}:
	default:
		relay.pendingDrop += uint64(len(payload))
		relay.totalDropped.Add(uint64(len(payload)))
	}
}

func (relay *nonBlockingRelay) flushMarker() {
	if relay.channel == nil {
		return
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.pendingDrop == 0 {
		return
	}
	select {
	case relay.channel <- OutputChunk{DroppedBytes: relay.pendingDrop}:
		relay.pendingDrop = 0
	default:
	}
}
