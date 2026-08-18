// Package conformance contains the transport-neutral test suite that every
// prototype adapter must pass. Keeping it outside either candidate prevents a
// candidate-specific behavior from silently becoming part of the contract.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/transport"
)

// Factory creates one connected Agent/Server pair. Each subtest receives a
// fresh pair so cancellation or protocol failures cannot leak between cases.
type Factory func(context.Context, transport.Handler, transport.Limits) (transport.Caller, func(), error)

const testTimeout = 5 * time.Second

// Run executes the common A.4 contract against one adapter.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("unary", func(t *testing.T) { testUnary(t, factory) })
	t.Run("session_identity", func(t *testing.T) { testSessionIdentity(t, factory) })
	t.Run("receive_order_and_outcome", func(t *testing.T) { testReceiveOrder(t, factory) })
	t.Run("duplex", func(t *testing.T) { testDuplex(t, factory) })
	t.Run("cancel_propagates", func(t *testing.T) { testCancel(t, factory) })
	t.Run("deadline_propagates", func(t *testing.T) { testDeadline(t, factory) })
	t.Run("message_limit", func(t *testing.T) { testMessageLimit(t, factory) })
	t.Run("response_message_limit", func(t *testing.T) { testResponseMessageLimit(t, factory) })
	t.Run("exchange_limit", func(t *testing.T) { testExchangeLimit(t, factory) })
	t.Run("stream_isolation", func(t *testing.T) { testStreamIsolation(t, factory) })
	t.Run("session_close", func(t *testing.T) { testSessionClose(t, factory) })
}

func testSessionIdentity(t *testing.T, factory Factory) {
	ctx1, first := pair(t, factory, transport.UnimplementedHandler{}, transport.DefaultLimits())
	_ = ctx1
	ctx2, second := pair(t, factory, transport.UnimplementedHandler{}, transport.DefaultLimits())
	_ = ctx2
	firstInfo, secondInfo := first.Info(), second.Info()
	if len(firstInfo.SessionID) != 32 || len(secondInfo.SessionID) != 32 {
		t.Fatalf("session IDs must encode 128 bits: %q %q", firstInfo.SessionID, secondInfo.SessionID)
	}
	if firstInfo.SessionID == secondInfo.SessionID {
		t.Fatalf("reconnect reused session ID %q", firstInfo.SessionID)
	}
	if firstInfo.AgentID == "" || firstInfo.ProtocolVersion != transport.ProtocolVersion {
		t.Fatalf("invalid negotiated session info: %+v", firstInfo)
	}
}

func pair(t *testing.T, factory Factory, h transport.Handler, limits transport.Limits) (context.Context, transport.Caller) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	caller, cleanup, err := factory(ctx, h, limits)
	if err != nil {
		cancel()
		t.Fatalf("create pair: %v", err)
	}
	t.Cleanup(func() {
		cleanup()
		cancel()
	})
	return ctx, caller
}

func testUnary(t *testing.T, factory Factory) {
	want := []byte("logical-payload")
	h := transport.HandlerFuncs{UnaryFunc: func(_ context.Context, call transport.Call) ([]byte, error) {
		if call.Method != "echo" || call.Class != transport.ClassQuery || !bytes.Equal(call.Payload, want) {
			t.Errorf("unexpected call: %+v", call)
		}
		return append([]byte("reply:"), call.Payload...), nil
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	got, err := caller.Invoke(ctx, transport.Call{Method: "echo", Class: transport.ClassQuery, Payload: want})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !bytes.Equal(got, append([]byte("reply:"), want...)) {
		t.Fatalf("reply = %q", got)
	}
}

func testReceiveOrder(t *testing.T, factory Factory) {
	h := transport.HandlerFuncs{ReceiveFunc: func(ctx context.Context, _ transport.Call, out transport.Sender) error {
		for _, msg := range [][]byte{[]byte("one"), []byte("two"), []byte("three")} {
			if err := out.Send(ctx, msg); err != nil {
				return err
			}
		}
		return nil
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	s, err := caller.OpenReceive(ctx, transport.Call{Method: "stream", Class: transport.ClassBulk})
	if err != nil {
		t.Fatalf("OpenReceive: %v", err)
	}
	for _, want := range []string{"one", "two", "three"} {
		got, err := s.Recv(ctx)
		if err != nil || string(got) != want {
			t.Fatalf("Recv = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := s.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv error = %v, want io.EOF", err)
	}
	a, ok := s.Outcome()
	if !ok || a.Status.Code != transport.CodeOK || a.Messages != 3 || a.Bytes != 11 {
		t.Fatalf("Outcome = %+v, %v", a, ok)
	}
	b, ok := s.Outcome()
	if !ok || b != a {
		t.Fatalf("Outcome changed: first=%+v second=%+v", a, b)
	}
}

func testDuplex(t *testing.T, factory Factory) {
	h := transport.HandlerFuncs{DuplexFunc: func(ctx context.Context, _ transport.Call, ch transport.Duplex) error {
		for {
			msg, err := ch.Recv(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ch.Send(ctx, append([]byte("ack:"), msg...)); err != nil {
				return err
			}
		}
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	s, err := caller.OpenDuplex(ctx, transport.Call{Method: "sync", Class: transport.ClassDurable})
	if err != nil {
		t.Fatalf("OpenDuplex: %v", err)
	}
	for _, msg := range []string{"a", "b"} {
		if err := s.Send(ctx, []byte(msg)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		got, err := s.Recv(ctx)
		if err != nil || string(got) != "ack:"+msg {
			t.Fatalf("Recv = %q, %v", got, err)
		}
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := s.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv = %v", err)
	}
}

func testCancel(t *testing.T, factory Factory) {
	seen := make(chan struct{})
	h := transport.HandlerFuncs{ReceiveFunc: func(ctx context.Context, _ transport.Call, _ transport.Sender) error {
		<-ctx.Done()
		close(seen)
		return ctx.Err()
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	s, err := caller.OpenReceive(ctx, transport.Call{Method: "wait", Class: transport.ClassControl})
	if err != nil {
		t.Fatalf("OpenReceive: %v", err)
	}
	s.Cancel(errors.New("test cancellation"))
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("cancellation did not reach responder")
	}
	_, err = s.Recv(ctx)
	if code := transport.StatusOf(err).Code; code != transport.CodeCanceled {
		t.Fatalf("Recv status = %s, want canceled (err=%v)", code, err)
	}
}

func testDeadline(t *testing.T, factory Factory) {
	h := transport.HandlerFuncs{UnaryFunc: func(ctx context.Context, _ transport.Call) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := caller.Invoke(deadline, transport.Call{Method: "deadline", Class: transport.ClassControl})
	if code := transport.StatusOf(err).Code; code != transport.CodeDeadlineExceeded {
		t.Fatalf("Invoke status = %s, want deadline_exceeded (err=%v)", code, err)
	}
}

func testMessageLimit(t *testing.T, factory Factory) {
	limits := transport.DefaultLimits()
	limits.MaxMessageBytes = 8
	ctx, caller := pair(t, factory, transport.HandlerFuncs{UnaryFunc: func(context.Context, transport.Call) ([]byte, error) {
		return []byte("should-not-run"), nil
	}}, limits)
	_, err := caller.Invoke(ctx, transport.Call{Method: "large", Class: transport.ClassQuery, Payload: make([]byte, 9)})
	if code := transport.StatusOf(err).Code; code != transport.CodeMessageTooLarge {
		t.Fatalf("Invoke status = %s, want message_too_large (err=%v)", code, err)
	}
}

func testResponseMessageLimit(t *testing.T, factory Factory) {
	limits := transport.DefaultLimits()
	limits.MaxMessageBytes = 8
	ctx, caller := pair(t, factory, transport.HandlerFuncs{UnaryFunc: func(context.Context, transport.Call) ([]byte, error) {
		return make([]byte, 9), nil
	}}, limits)
	_, err := caller.Invoke(ctx, transport.Call{Method: "large-response", Class: transport.ClassQuery})
	if code := transport.StatusOf(err).Code; code != transport.CodeMessageTooLarge {
		t.Fatalf("Invoke status = %s, want message_too_large (err=%v)", code, err)
	}
}

func testExchangeLimit(t *testing.T, factory Factory) {
	started := make(chan struct{})
	var once sync.Once
	h := transport.HandlerFuncs{ReceiveFunc: func(ctx context.Context, _ transport.Call, _ transport.Sender) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}}
	limits := transport.DefaultLimits()
	limits.MaxConcurrentExchanges = 1
	ctx, caller := pair(t, factory, h, limits)
	first, err := caller.OpenReceive(ctx, transport.Call{Method: "first", Class: transport.ClassBulk})
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.Cancel(nil)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first exchange did not start")
	}
	_, err = caller.OpenReceive(ctx, transport.Call{Method: "second", Class: transport.ClassBulk})
	if code := transport.StatusOf(err).Code; code != transport.CodeResourceExhausted {
		t.Fatalf("second exchange status = %s, want resource_exhausted (err=%v)", code, err)
	}
}

func testStreamIsolation(t *testing.T, factory Factory) {
	blockedStarted := make(chan struct{})
	var once sync.Once
	h := transport.HandlerFuncs{
		ReceiveFunc: func(ctx context.Context, call transport.Call, out transport.Sender) error {
			if call.Method == "blocked" {
				for i := 0; ; i++ {
					once.Do(func() { close(blockedStarted) })
					if err := out.Send(ctx, make([]byte, 1024)); err != nil {
						return err
					}
				}
			}
			return out.Send(ctx, []byte("independent"))
		},
		UnaryFunc: func(context.Context, transport.Call) ([]byte, error) { return []byte("control"), nil },
	}
	limits := transport.DefaultLimits()
	limits.MaxMessageBytes = 2048
	ctx, caller := pair(t, factory, h, limits)
	blocked, err := caller.OpenReceive(ctx, transport.Call{Method: "blocked", Class: transport.ClassBulk})
	if err != nil {
		t.Fatalf("open blocked stream: %v", err)
	}
	defer blocked.Cancel(nil)
	select {
	case <-blockedStarted:
	case <-ctx.Done():
		t.Fatal("blocked producer did not start")
	}

	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	got, err := caller.Invoke(deadline, transport.Call{Method: "control", Class: transport.ClassControl})
	if err != nil || string(got) != "control" {
		t.Fatalf("control behind blocked stream = %q, %v", got, err)
	}
	active, err := caller.OpenReceive(deadline, transport.Call{Method: "active", Class: transport.ClassBulk})
	if err != nil {
		t.Fatalf("open independent stream: %v", err)
	}
	got, err = active.Recv(deadline)
	if err != nil || string(got) != "independent" {
		t.Fatalf("independent stream = %q, %v", got, err)
	}
	durable, err := caller.OpenReceive(deadline, transport.Call{Method: "durable", Class: transport.ClassDurable})
	if err != nil {
		t.Fatalf("open durable stream: %v", err)
	}
	got, err = durable.Recv(deadline)
	if err != nil || string(got) != "independent" {
		t.Fatalf("durable stream = %q, %v", got, err)
	}
}

func testSessionClose(t *testing.T, factory Factory) {
	var canceled atomic.Bool
	h := transport.HandlerFuncs{ReceiveFunc: func(ctx context.Context, _ transport.Call, _ transport.Sender) error {
		<-ctx.Done()
		canceled.Store(true)
		return ctx.Err()
	}}
	ctx, caller := pair(t, factory, h, transport.DefaultLimits())
	s, err := caller.OpenReceive(ctx, transport.Call{Method: "wait", Class: transport.ClassBulk})
	if err != nil {
		t.Fatalf("OpenReceive: %v", err)
	}
	if err := caller.Close(errors.New("closed by test")); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-caller.Done():
	case <-ctx.Done():
		t.Fatal("session Done did not close")
	}
	_, err = s.Recv(ctx)
	if code := transport.StatusOf(err).Code; code != transport.CodeUnavailable {
		t.Fatalf("stream status = %s, want unavailable", code)
	}
	deadline := time.Now().Add(time.Second)
	for !canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !canceled.Load() {
		t.Fatal("session close did not cancel responder")
	}
}
