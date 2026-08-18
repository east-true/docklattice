package grpcadapter

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	channelzpb "google.golang.org/grpc/channelz/grpc_channelz_v1"

	pb "github.com/east-true/dockpilot/internal/candidate/grpcadapter/pb"
	"github.com/east-true/dockpilot/internal/transport"
)

type sessionState struct {
	info transport.SessionInfo
	done chan struct{}
	once sync.Once
	mu   sync.RWMutex
	err  error
}

func newSessionState(info transport.SessionInfo) sessionState {
	return sessionState{info: info, done: make(chan struct{})}
}

func (s *sessionState) Info() transport.SessionInfo { return s.info }
func (s *sessionState) Done() <-chan struct{}       { return s.done }
func (s *sessionState) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}
func (s *sessionState) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

type agentSession struct {
	sessionState
	conn       net.Conn
	server     *grpc.Server
	listen     *singleConnListener
	dispatcher *sendDispatcher
}

func newAgentSession(info transport.SessionInfo, conn net.Conn, server *grpc.Server, listen *singleConnListener, dispatcher *sendDispatcher) *agentSession {
	return &agentSession{sessionState: newSessionState(info), conn: conn, server: server, listen: listen, dispatcher: dispatcher}
}

func (s *agentSession) Close(cause error) error {
	s.server.Stop()
	s.dispatcher.stop()
	_ = s.listen.Close()
	s.finish(cause)
	return nil
}

type callerSession struct {
	sessionState
	cc     *grpc.ClientConn
	conn   net.Conn
	limits transport.Limits
	nextID atomic.Uint64
	slots  chan struct{}
}

func newCallerSession(info transport.SessionInfo, cc *grpc.ClientConn, conn net.Conn, limits transport.Limits) *callerSession {
	s := &callerSession{sessionState: newSessionState(info), cc: cc, conn: conn, limits: limits}
	if limits.MaxConcurrentExchanges > 0 {
		s.slots = make(chan struct{}, limits.MaxConcurrentExchanges)
	}
	return s
}

func (s *callerSession) Close(cause error) error {
	s.finish(cause)
	err := s.cc.Close()
	_ = s.conn.Close()
	return err
}

func (s *callerSession) validate(call transport.Call) error {
	if !call.Class.Valid() {
		return transport.Errorf(transport.CodeProtocol, "invalid traffic class %d", call.Class)
	}
	if len(call.Payload) > s.limits.MaxMessageBytes {
		return transport.Errorf(transport.CodeMessageTooLarge, "message is %d bytes; limit is %d", len(call.Payload), s.limits.MaxMessageBytes)
	}
	select {
	case <-s.done:
		return transport.Errorf(transport.CodeUnavailable, "session closed")
	default:
		return nil
	}
}

func (s *callerSession) open(call transport.Call, kind transport.Kind) (*pb.OpenRequest, func(), error) {
	if err := s.validate(call); err != nil {
		return nil, nil, err
	}
	release := func() {}
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			var once sync.Once
			release = func() { once.Do(func() { <-s.slots }) }
		default:
			return nil, nil, transport.Errorf(transport.CodeResourceExhausted, "concurrent exchange limit reached")
		}
	}
	return &pb.OpenRequest{ExchangeId: s.nextID.Add(1), Method: string(call.Method), Kind: uint32(kind), TrafficClass: uint32(call.Class), Payload: call.Payload}, release, nil
}

func (s *callerSession) Invoke(ctx context.Context, call transport.Call) ([]byte, error) {
	req, release, err := s.open(call, transport.KindUnary)
	if err != nil {
		return nil, err
	}
	defer release()
	var resp pb.UnaryResponse
	if err := s.cc.Invoke(ctx, unaryMethod, req, &resp); err != nil {
		return nil, fromGRPC(err)
	}
	if len(resp.Payload) > s.limits.MaxMessageBytes {
		return nil, transport.Errorf(transport.CodeMessageTooLarge, "response exceeds message limit")
	}
	return resp.Payload, nil
}

func (s *callerSession) OpenReceive(ctx context.Context, call transport.Call) (transport.ReceiveStream, error) {
	req, release, err := s.open(call, transport.KindReceive)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancelCause(ctx)
	cs, err := s.cc.NewStream(streamCtx, &serviceDesc.Streams[0], receiveMethod)
	if err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	if err := cs.SendMsg(req); err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	if err := cs.CloseSend(); err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	if _, err := cs.Header(); err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	return &receiveStream{id: transport.ExchangeID(req.ExchangeId), stream: cs, cancel: cancel, limits: s.limits, sessionDone: s.done, release: release}, nil
}

func (s *callerSession) OpenDuplex(ctx context.Context, call transport.Call) (transport.DuplexStream, error) {
	req, release, err := s.open(call, transport.KindDuplex)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancelCause(ctx)
	cs, err := s.cc.NewStream(streamCtx, &serviceDesc.Streams[1], duplexMethod)
	if err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	if err := cs.SendMsg(&pb.StreamFrame{Body: &pb.StreamFrame_Open{Open: req}}); err != nil {
		release()
		cancel(err)
		return nil, fromGRPC(err)
	}
	return &duplexStream{receiveStream: receiveStream{id: transport.ExchangeID(req.ExchangeId), stream: cs, cancel: cancel, limits: s.limits, sessionDone: s.done, release: release}}, nil
}

// CandidateMetrics exposes Candidate A's HTTP/2 flow-control evidence through
// the standard gRPC channelz service. These names never enter the common
// transport contract.
func (s *callerSession) CandidateMetrics(ctx context.Context) map[string]float64 {
	return s.channelzMetrics(ctx, 1)
}

func (s *callerSession) CandidateRecoveryMetrics(ctx context.Context) map[string]float64 {
	// A channelz response is transported on this same HTTP/2 connection and
	// therefore consumes the window it reports. Probe through at least one
	// WINDOW_UPDATE cycle and retain the observed peak so the observer's own
	// traffic is not misclassified as leaked application credit.
	return s.channelzMetrics(ctx, 1024)
}

func (s *callerSession) channelzMetrics(ctx context.Context, probes int) map[string]float64 {
	out := map[string]float64{"grpc_active_exchanges": float64(len(s.slots))}
	client := channelzpb.NewChannelzClient(s.cc)
	// A channelz response itself consumes the window it reports. Repeating a
	// bounded probe crosses gRPC's WINDOW_UPDATE threshold; we retain both the
	// first observation and the recovery peak so the measurement does not
	// mistake its own sub-threshold response bytes for a leak.
	for probe := 0; probe < probes; probe++ {
		servers, err := client.GetServers(ctx, &channelzpb.GetServersRequest{})
		if err != nil {
			return out
		}
		for _, server := range servers.Server {
			sockets, err := client.GetServerSockets(ctx, &channelzpb.GetServerSocketsRequest{ServerId: server.Ref.ServerId})
			if err != nil {
				continue
			}
			for _, ref := range sockets.SocketRef {
				socket, err := client.GetSocket(ctx, &channelzpb.GetSocketRequest{SocketId: ref.SocketId})
				if err != nil || socket.Socket == nil || socket.Socket.Data == nil {
					continue
				}
				data := socket.Socket.Data
				if data.LocalFlowControlWindow != nil {
					value := float64(data.LocalFlowControlWindow.Value)
					if probe == 0 {
						out["grpc_local_flow_control_window_bytes"] = value
					}
					out["grpc_local_flow_control_window_recovery_peak_bytes"] = max(out["grpc_local_flow_control_window_recovery_peak_bytes"], value)
				}
				if data.RemoteFlowControlWindow != nil {
					value := float64(data.RemoteFlowControlWindow.Value)
					if probe == 0 {
						out["grpc_remote_flow_control_window_bytes"] = value
					}
					out["grpc_remote_flow_control_window_recovery_peak_bytes"] = max(out["grpc_remote_flow_control_window_recovery_peak_bytes"], value)
				}
				out["grpc_streams_started_total"] = float64(data.StreamsStarted)
				out["grpc_streams_succeeded_total"] = float64(data.StreamsSucceeded)
				out["grpc_streams_failed_total"] = float64(data.StreamsFailed)
			}
		}
	}
	return out
}

type receiveStream struct {
	id          transport.ExchangeID
	stream      grpc.ClientStream
	cancel      context.CancelCauseFunc
	limits      transport.Limits
	sessionDone <-chan struct{}
	release     func()

	mu       sync.RWMutex
	outcome  transport.Outcome
	terminal bool
	canceled atomic.Bool
}

func (s *receiveStream) ID() transport.ExchangeID { return s.id }

func (s *receiveStream) Recv(ctx context.Context) ([]byte, error) {
	type result struct {
		frame *pb.StreamFrame
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var frame pb.StreamFrame
		err := s.stream.RecvMsg(&frame)
		ch <- result{&frame, err}
	}()
	select {
	case <-ctx.Done():
		s.cancel(ctx.Err())
		st := transport.StatusOf(ctx.Err())
		s.setTerminal(st)
		return nil, st.Err()
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				s.setTerminal(transport.Status{Code: transport.CodeOK})
				return nil, io.EOF
			}
			st := transport.StatusOf(fromGRPC(r.err))
			select {
			case <-s.sessionDone:
				st = transport.Status{Code: transport.CodeUnavailable, Reason: "session closed", Cause: r.err}
			default:
			}
			if s.canceled.Load() {
				st = transport.Status{Code: transport.CodeCanceled, Reason: "stream canceled", Cause: r.err}
			}
			s.setTerminal(st)
			return nil, st.Err()
		}
		payload, ok := r.frame.Body.(*pb.StreamFrame_Payload)
		if !ok {
			st := transport.Status{Code: transport.CodeProtocol, Reason: "expected payload frame"}
			s.setTerminal(st)
			return nil, st.Err()
		}
		if len(payload.Payload) > s.limits.MaxMessageBytes {
			st := transport.Status{Code: transport.CodeMessageTooLarge, Reason: "stream message exceeds limit"}
			s.setTerminal(st)
			s.Cancel(st.Err())
			return nil, st.Err()
		}
		s.mu.Lock()
		s.outcome.Messages++
		s.outcome.Bytes += uint64(len(payload.Payload))
		s.mu.Unlock()
		return payload.Payload, nil
	}
}

func (s *receiveStream) Cancel(cause error) {
	s.canceled.Store(true)
	s.cancel(cause)
	s.setTerminal(transport.Status{Code: transport.CodeCanceled, Reason: "stream canceled", Cause: cause})
}

func (s *receiveStream) Outcome() (transport.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outcome, s.terminal
}

func (s *receiveStream) setTerminal(status transport.Status) {
	s.mu.Lock()
	if !s.terminal {
		s.outcome.Status = status
		s.terminal = true
		if s.release != nil {
			s.release()
		}
	}
	s.mu.Unlock()
}

type duplexStream struct{ receiveStream }

func (s *duplexStream) Send(ctx context.Context, msg []byte) error {
	if len(msg) > s.limits.MaxMessageBytes {
		return transport.Errorf(transport.CodeMessageTooLarge, "stream message exceeds limit")
	}
	if err := s.stream.SendMsg(&pb.StreamFrame{Body: &pb.StreamFrame_Payload{Payload: msg}}); err != nil {
		return fromGRPC(err)
	}
	return nil
}

func (s *duplexStream) CloseSend() error { return fromGRPC(s.stream.CloseSend()) }
