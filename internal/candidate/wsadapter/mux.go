package wsadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/east-true/dockpilot/internal/transport"
)

type mux struct {
	info    transport.SessionInfo
	conn    net.Conn
	handler transport.Handler
	caller  bool
	limits  transport.Limits
	sched   *scheduler

	ctx           context.Context
	cancel        context.CancelCauseFunc
	done          chan struct{}
	once          sync.Once
	errMu         sync.RWMutex
	err           error
	nextID        atomic.Uint64
	bufferedBytes atomic.Int64

	mu      sync.RWMutex
	streams map[uint64]*wireStream
}

func newMux(conn net.Conn, info transport.SessionInfo, h transport.Handler, caller bool) *mux {
	ctx, cancel := context.WithCancelCause(context.Background())
	m := &mux{info: info, conn: conn, handler: h, caller: caller, limits: info.Limits, ctx: ctx, cancel: cancel, done: make(chan struct{}), streams: make(map[uint64]*wireStream)}
	m.sched = newScheduler(conn)
	go m.readLoop()
	go func() {
		select {
		case err := <-m.sched.err:
			m.finish(transport.Wrap(transport.CodeUnavailable, err, "WebSocket writer"))
		case <-m.done:
		}
	}()
	return m
}

func (m *mux) Info() transport.SessionInfo { return m.info }
func (m *mux) Done() <-chan struct{}       { return m.done }
func (m *mux) Err() error {
	m.errMu.RLock()
	defer m.errMu.RUnlock()
	return m.err
}
func (m *mux) Close(cause error) error {
	m.finish(cause)
	return nil
}

func (m *mux) finish(err error) {
	m.once.Do(func() {
		if err == nil {
			err = transport.Errorf(transport.CodeUnavailable, "session closed")
		}
		m.errMu.Lock()
		m.err = err
		m.errMu.Unlock()
		m.cancel(err)
		m.sched.close()
		_ = m.conn.SetDeadline(time.Now())
		_ = m.conn.Close()
		m.mu.Lock()
		for _, stream := range m.streams {
			stream.finish(transport.Status{Code: transport.CodeUnavailable, Reason: "session closed", Cause: err})
		}
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *mux) validate(call transport.Call) error {
	if !m.caller {
		return transport.Errorf(transport.CodeProtocol, "Agent session cannot initiate exchanges")
	}
	if !call.Class.Valid() {
		return transport.Errorf(transport.CodeProtocol, "invalid traffic class %d", call.Class)
	}
	if len(call.Payload) > m.limits.MaxMessageBytes {
		return transport.Errorf(transport.CodeMessageTooLarge, "message is %d bytes; limit is %d", len(call.Payload), m.limits.MaxMessageBytes)
	}
	select {
	case <-m.done:
		return transport.Errorf(transport.CodeUnavailable, "session closed")
	default:
		return nil
	}
}

func (m *mux) newStream(ctx context.Context, call transport.Call, kind transport.Kind) (*wireStream, error) {
	if err := m.validate(call); err != nil {
		return nil, err
	}
	payload, err := encodeOpen(call)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.limits.MaxConcurrentExchanges > 0 && len(m.streams) >= m.limits.MaxConcurrentExchanges {
		m.mu.Unlock()
		return nil, transport.Errorf(transport.CodeResourceExhausted, "concurrent exchange limit reached")
	}
	id := m.nextID.Add(1)
	stream := newWireStream(m, id, call.Class, kind, ctx)
	m.streams[id] = stream
	m.mu.Unlock()
	if err := m.sched.enqueue(ctx, frame{streamID: id, typ: frameOpen, class: call.Class, aux: uint8(kind), payload: payload}); err != nil {
		m.remove(id)
		return nil, mapNetError(err)
	}
	// Grant the Agent credit for its response direction.
	if err := m.grant(ctx, stream, initialCreditByte, initialCreditMsgs); err != nil {
		m.remove(id)
		return nil, err
	}
	select {
	case <-stream.ready:
	case <-ctx.Done():
		stream.Cancel(ctx.Err())
		return nil, transport.Wrap(transport.StatusOf(ctx.Err()).Code, ctx.Err(), "open exchange canceled")
	case <-m.done:
		return nil, transport.Errorf(transport.CodeUnavailable, "session closed while opening exchange")
	}
	return stream, nil
}

func (m *mux) Invoke(ctx context.Context, call transport.Call) ([]byte, error) {
	s, err := m.newStream(ctx, call, transport.KindUnary)
	if err != nil {
		return nil, err
	}
	defer m.remove(s.id)
	payload, err := s.Recv(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.Recv(ctx); !errors.Is(err, io.EOF) {
		return nil, err
	}
	return payload, nil
}

func (m *mux) OpenReceive(ctx context.Context, call transport.Call) (transport.ReceiveStream, error) {
	return m.newStream(ctx, call, transport.KindReceive)
}

func (m *mux) OpenDuplex(ctx context.Context, call transport.Call) (transport.DuplexStream, error) {
	return m.newStream(ctx, call, transport.KindDuplex)
}

// CandidateMetrics exposes Candidate B's scheduler queues and outstanding
// per-stream credits without introducing either concept into transport.Session.
func (m *mux) CandidateMetrics(context.Context) map[string]float64 {
	out := make(map[string]float64, 9+transport.NumClasses)
	out["websocket_credit_bytes_available"] = 0
	out["websocket_credit_messages_available"] = 0
	out["websocket_stream_credit_bytes_min"] = math.MaxFloat64
	out["websocket_stream_credit_bytes_max"] = 0
	out["websocket_stream_credit_messages_min"] = math.MaxFloat64
	out["websocket_stream_credit_messages_max"] = 0
	out["websocket_send_queue_bytes"] = float64(m.sched.queueBytes())
	m.mu.RLock()
	out["websocket_active_streams"] = float64(len(m.streams))
	out["websocket_receive_buffer_bytes"] = float64(m.bufferedBytes.Load())
	out["websocket_buffer_bytes"] = out["websocket_send_queue_bytes"] + out["websocket_receive_buffer_bytes"]
	for _, stream := range m.streams {
		stream.creditMu.Lock()
		creditBytes := float64(stream.creditByte)
		creditMessages := float64(stream.creditMsgs)
		out["websocket_credit_bytes_available"] += creditBytes
		out["websocket_credit_messages_available"] += creditMessages
		out["websocket_stream_credit_bytes_min"] = min(out["websocket_stream_credit_bytes_min"], creditBytes)
		out["websocket_stream_credit_bytes_max"] = max(out["websocket_stream_credit_bytes_max"], creditBytes)
		out["websocket_stream_credit_messages_min"] = min(out["websocket_stream_credit_messages_min"], creditMessages)
		out["websocket_stream_credit_messages_max"] = max(out["websocket_stream_credit_messages_max"], creditMessages)
		stream.creditMu.Unlock()
	}
	m.mu.RUnlock()
	if out["websocket_active_streams"] == 0 {
		out["websocket_stream_credit_bytes_min"] = 0
		out["websocket_stream_credit_messages_min"] = 0
	}
	for i := 0; i < transport.NumClasses; i++ {
		out[fmt.Sprintf("websocket_send_queue_p%d_frames", i)] = float64(m.sched.queueLen(transport.Class(i)))
	}
	return out
}

func (m *mux) readLoop() {
	for {
		f, err := readFrame(m.conn, m.limits.MaxMessageBytes+maxMethodBytes+2)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				m.finish(mapNetError(err))
			} else {
				m.finish(transport.Errorf(transport.CodeUnavailable, "connection closed"))
			}
			return
		}
		if !f.class.Valid() {
			m.finish(transport.Errorf(transport.CodeProtocol, "invalid frame class"))
			return
		}
		if f.typ == frameOpen {
			if m.caller {
				m.finish(transport.Errorf(transport.CodeProtocol, "caller received OPEN"))
				return
			}
			m.acceptOpen(f)
			continue
		}
		m.mu.RLock()
		s := m.streams[f.streamID]
		m.mu.RUnlock()
		if s == nil {
			// Late CREDIT/CLOSE after local cancellation is harmless.
			continue
		}
		if f.typ == frameCancel {
			if f.class != transport.ClassControl {
				m.finish(transport.Errorf(transport.CodeProtocol, "CANCEL must use P0 control"))
				return
			}
		} else if f.class != s.class {
			m.finish(transport.Errorf(transport.CodeProtocol, "frame class changed within stream"))
			return
		}
		switch f.typ {
		case frameData:
			if len(f.payload) > m.limits.MaxMessageBytes {
				m.finish(transport.Errorf(transport.CodeMessageTooLarge, "DATA exceeds limit"))
				return
			}
			s.recvMu.Lock()
			select {
			case <-s.ctx.Done():
				s.recvMu.Unlock()
				continue
			default:
			}
			m.bufferedBytes.Add(int64(len(f.payload)))
			select {
			case s.recv <- f.payload:
			case <-s.ctx.Done():
				m.bufferedBytes.Add(-int64(len(f.payload)))
			case <-m.done:
				m.bufferedBytes.Add(-int64(len(f.payload)))
				s.recvMu.Unlock()
				return
			}
			s.recvMu.Unlock()
		case frameCredit:
			s.addCredit(f.creditByte, f.creditMsgs)
			if m.caller {
				s.markReady()
			}
		case frameCancel:
			s.cancel(transport.Errorf(transport.CodeCanceled, "remote canceled stream"))
		case frameClose:
			if f.aux == halfCloseAux {
				s.closeReceive()
			} else {
				if transport.Code(f.aux) > transport.CodeInternal {
					m.finish(transport.Errorf(transport.CodeProtocol, "invalid CLOSE status %d", f.aux))
					return
				}
				s.finish(transport.Status{Code: transport.Code(f.aux), Reason: string(f.payload)})
				if m.caller {
					m.remove(s.id)
				}
			}
		case framePing:
		default:
			m.finish(transport.Errorf(transport.CodeProtocol, "unknown frame type %d", f.typ))
			return
		}
	}
}

func (m *mux) acceptOpen(f frame) {
	method, payload, err := decodeOpen(f.payload)
	if err != nil {
		m.finish(err)
		return
	}
	kind := transport.Kind(f.aux)
	if kind > transport.KindDuplex || len(payload) > m.limits.MaxMessageBytes {
		m.finish(transport.Errorf(transport.CodeProtocol, "invalid OPEN"))
		return
	}
	m.mu.Lock()
	if _, exists := m.streams[f.streamID]; exists {
		m.mu.Unlock()
		m.finish(transport.Errorf(transport.CodeProtocol, "duplicate stream id"))
		return
	}
	stream := newWireStream(m, f.streamID, f.class, kind, m.ctx)
	m.streams[f.streamID] = stream
	m.mu.Unlock()
	// Grant caller-to-Agent credit, which is used only by duplex streams.
	_ = m.grant(m.ctx, stream, initialCreditByte, initialCreditMsgs)
	go m.dispatch(stream, transport.Call{Method: method, Class: f.class, Payload: payload})
}

func (m *mux) dispatch(s *wireStream, call transport.Call) {
	var err error
	switch s.kind {
	case transport.KindUnary:
		var response []byte
		response, err = m.handler.Unary(s.ctx, call)
		if err == nil {
			err = s.Send(s.ctx, response)
		}
	case transport.KindReceive:
		err = m.handler.Receive(s.ctx, call, s)
	case transport.KindDuplex:
		err = m.handler.Duplex(s.ctx, call, s)
	default:
		err = transport.Errorf(transport.CodeProtocol, "invalid exchange kind")
	}
	status := transport.StatusOf(err)
	if errors.Is(err, context.Canceled) {
		status = transport.Status{Code: transport.CodeCanceled, Reason: "exchange canceled", Cause: err}
	}
	_ = m.sched.enqueue(m.ctx, frame{streamID: s.id, typ: frameClose, class: s.class, aux: uint8(status.Code), payload: []byte(status.Reason)})
	s.finish(status)
	m.remove(s.id)
}

func (m *mux) grant(ctx context.Context, s *wireStream, bytes, messages uint32) error {
	return mapNetError(m.sched.enqueue(ctx, frame{streamID: s.id, typ: frameCredit, class: s.class, creditByte: bytes, creditMsgs: messages}))
}

func (m *mux) remove(id uint64) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

type wireStream struct {
	mux    *mux
	id     uint64
	class  transport.Class
	kind   transport.Kind
	ctx    context.Context
	cancel context.CancelCauseFunc
	recv   chan []byte
	recvMu sync.Mutex

	creditMu   sync.Mutex
	creditByte uint64
	creditMsgs uint64
	creditWake chan struct{}

	termMu      sync.RWMutex
	terminal    bool
	outcome     transport.Outcome
	inboundOnce sync.Once
	inboundDone chan struct{}
	termOnce    sync.Once
	termDone    chan struct{}
	readyOnce   sync.Once
	ready       chan struct{}
	canceled    atomic.Bool
}

func newWireStream(m *mux, id uint64, class transport.Class, kind transport.Kind, parent context.Context) *wireStream {
	ctx, cancel := context.WithCancelCause(parent)
	return &wireStream{mux: m, id: id, class: class, kind: kind, ctx: ctx, cancel: cancel, recv: make(chan []byte, initialCreditMsgs), creditWake: make(chan struct{}, 1), inboundDone: make(chan struct{}), termDone: make(chan struct{}), ready: make(chan struct{})}
}

func (s *wireStream) ID() transport.ExchangeID { return transport.ExchangeID(s.id) }

func (s *wireStream) Send(ctx context.Context, msg []byte) error {
	if len(msg) > s.mux.limits.MaxMessageBytes {
		return transport.Errorf(transport.CodeMessageTooLarge, "stream message exceeds limit")
	}
	for {
		s.creditMu.Lock()
		if s.creditMsgs > 0 && s.creditByte >= uint64(len(msg)) {
			s.creditMsgs--
			s.creditByte -= uint64(len(msg))
			s.creditMu.Unlock()
			break
		}
		s.creditMu.Unlock()
		select {
		case <-ctx.Done():
			return transport.Wrap(transport.StatusOf(ctx.Err()).Code, ctx.Err(), "send canceled")
		case <-s.ctx.Done():
			return transport.Wrap(transport.CodeCanceled, context.Cause(s.ctx), "stream closed")
		case <-s.creditWake:
		}
	}
	return mapNetError(s.mux.sched.enqueue(ctx, frame{streamID: s.id, typ: frameData, class: s.class, payload: msg}))
}

func (s *wireStream) Recv(ctx context.Context) ([]byte, error) {
	// Preserve wire order by draining already queued DATA before observing the
	// terminal signal that followed it.
	select {
	case msg := <-s.recv:
		return s.consume(ctx, msg)
	default:
	}
	select {
	case <-ctx.Done():
		st := transport.StatusOf(ctx.Err())
		if !s.canceled.Swap(true) {
			_ = s.mux.sched.enqueue(context.Background(), frame{streamID: s.id, typ: frameCancel, class: transport.ClassControl, payload: []byte(ctx.Err().Error())})
		}
		s.cancel(ctx.Err())
		s.finish(st)
		s.mux.remove(s.id)
		return nil, st.Err()
	case msg := <-s.recv:
		return s.consume(ctx, msg)
	case <-s.inboundDone:
		return nil, io.EOF
	case <-s.termDone:
		select {
		case msg := <-s.recv:
			return s.consume(ctx, msg)
		default:
		}
		outcome, _ := s.Outcome()
		if outcome.Status.Code == transport.CodeOK {
			return nil, io.EOF
		}
		return nil, outcome.Status.Err()
	}
}

func (s *wireStream) consume(ctx context.Context, msg []byte) ([]byte, error) {
	s.mux.bufferedBytes.Add(-int64(len(msg)))
	_ = s.mux.grant(ctx, s, uint32(len(msg)), 1)
	s.termMu.Lock()
	s.outcome.Messages++
	s.outcome.Bytes += uint64(len(msg))
	s.termMu.Unlock()
	return msg, nil
}

func (s *wireStream) Cancel(cause error) {
	if s.canceled.Swap(true) {
		return
	}
	_ = s.mux.sched.enqueue(context.Background(), frame{streamID: s.id, typ: frameCancel, class: transport.ClassControl, payload: []byte(errorString(cause))})
	s.cancel(cause)
	s.finish(transport.Status{Code: transport.CodeCanceled, Reason: "stream canceled", Cause: cause})
	s.mux.remove(s.id)
}

func (s *wireStream) Outcome() (transport.Outcome, bool) {
	s.termMu.RLock()
	defer s.termMu.RUnlock()
	return s.outcome, s.terminal
}

func (s *wireStream) CloseSend() error {
	return mapNetError(s.mux.sched.enqueue(s.ctx, frame{streamID: s.id, typ: frameClose, class: s.class, aux: halfCloseAux}))
}

func (s *wireStream) closeReceive() { s.inboundOnce.Do(func() { close(s.inboundDone) }) }
func (s *wireStream) markReady()    { s.readyOnce.Do(func() { close(s.ready) }) }

func (s *wireStream) finish(status transport.Status) {
	s.termMu.Lock()
	if !s.terminal {
		s.outcome.Status = status
		s.terminal = true
	}
	s.termMu.Unlock()
	s.termOnce.Do(func() { close(s.termDone) })
	if status.Code != transport.CodeOK {
		s.cancel(status.Err())
		s.recvMu.Lock()
		defer s.recvMu.Unlock()
		for {
			select {
			case msg := <-s.recv:
				s.mux.bufferedBytes.Add(-int64(len(msg)))
			default:
				return
			}
		}
	}
}

func (s *wireStream) addCredit(bytes, messages uint32) {
	s.creditMu.Lock()
	s.creditByte += uint64(bytes)
	s.creditMsgs += uint64(messages)
	s.creditMu.Unlock()
	select {
	case s.creditWake <- struct{}{}:
	default:
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mapNetError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if transport.StatusOf(err).Code != transport.CodeInternal {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return transport.Wrap(transport.CodeCanceled, err, "context canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return transport.Wrap(transport.CodeDeadlineExceeded, err, "deadline exceeded")
	}
	return transport.Wrap(transport.CodeUnavailable, err, "WebSocket transport")
}
