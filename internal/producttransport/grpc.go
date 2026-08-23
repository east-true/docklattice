package producttransport

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/east-true/dockpilot/internal/producttransport/pb"
)

const productALPN = "dockpilot-product-grpc/1"

const (
	heartbeatMethod            = "/dockpilot.product.v1.AgentControl/Heartbeat"
	queryMethod                = "/dockpilot.product.v1.AgentControl/Query"
	operationMethod            = "/dockpilot.product.v1.AgentControl/StartOperation"
	getOperationMethod         = "/dockpilot.product.v1.AgentControl/GetOperation"
	cancelOperationMethod      = "/dockpilot.product.v1.AgentControl/CancelOperation"
	listActiveOperationsMethod = "/dockpilot.product.v1.AgentControl/ListActiveOperations"
	auditMethod                = "/dockpilot.product.v1.AgentControl/SyncAudit"
	logsMethod                 = "/dockpilot.product.v1.AgentControl/StreamLogs"
	statsMethod                = "/dockpilot.product.v1.AgentControl/StreamStats"
	metricsMatrixMethod        = "/dockpilot.product.v1.AgentControl/StreamMetricsMatrix"
)

const (
	auditStreamIndex = iota
	logsStreamIndex
	statsStreamIndex
	metricsMatrixStreamIndex
)

type AgentConfig struct {
	Address     string
	TLSConfig   *tls.Config
	Credential  []byte
	Incarnation uint64
	// ProtocolVersion defaults to release N. N-1 is accepted only for the
	// explicit compatibility fixture and a real previous-release Agent.
	ProtocolVersion    uint32
	Clock              Clock
	MaxCredentialBytes int
	MaxMessageBytes    int
	HandshakeTimeout   time.Duration
	TickerFactory      TickerFactory
	// PeerSilenceTimeout closes a session whose Server has stopped calling.
	// The Server heartbeats on P0 and declares an Agent offline after a fixed
	// window; this is the same judgement from the Agent's side, and without it
	// a Server that disappears without closing the connection leaves the
	// session readable forever and the Agent never reconnects.
	PeerSilenceTimeout time.Duration
}

type AgentConnector struct{ config AgentConfig }

func NewAgentConnector(config AgentConfig) (*AgentConnector, error) {
	if config.Address == "" || config.TLSConfig == nil || len(config.Credential) == 0 || config.Incarnation == 0 {
		return nil, fmt.Errorf("agent address, TLS, credential, and non-zero incarnation are required")
	}
	if err := validateProductTLSConfig(config.TLSConfig); err != nil {
		return nil, err
	}
	applyAgentDefaults(&config)
	if !supportedProductProtocolVersion(config.ProtocolVersion) {
		return nil, fmt.Errorf("%w: unsupported Agent protocol version %d", ErrProtocol, config.ProtocolVersion)
	}
	config.Credential = append([]byte(nil), config.Credential...)
	return &AgentConnector{config: config}, nil
}

func applyAgentDefaults(config *AgentConfig) {
	if config.ProtocolVersion == 0 {
		config.ProtocolVersion = CurrentProductProtocolVersion
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.MaxCredentialBytes == 0 {
		config.MaxCredentialBytes = DefaultMaxCredentialBytes
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = DefaultHandshakeTimeout
	}
}

// Connect dials outward from the Agent, authenticates, then serves product
// gRPC calls on that same TLS connection.
func (c *AgentConnector) Connect(ctx context.Context, handler AgentHandler) (Session, error) {
	if handler == nil {
		return nil, errors.New("agent handler is required")
	}
	if c.config.MaxCredentialBytes <= 0 || c.config.MaxMessageBytes <= 0 || c.config.HandshakeTimeout <= 0 {
		return nil, errors.New("Agent transport limits must be positive")
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.config.Address)
	if err != nil {
		return nil, fmt.Errorf("dial Server: %w", err)
	}
	tlsConfig := productTLSConfig(c.config.TLSConfig)
	conn := tls.Client(raw, tlsConfig)
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, c.config.HandshakeTimeout)
	defer cancelHandshake()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	if conn.ConnectionState().NegotiatedProtocol != productALPN {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: product ALPN was not negotiated", ErrProtocol)
	}
	info, err := agentHandshakeVersion(
		handshakeCtx, conn, c.config.Credential, c.config.Incarnation,
		c.config.MaxCredentialBytes, c.config.ProtocolVersion,
	)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticate Agent session: %w", err)
	}
	one := newSingleConnListener(conn)
	watchdog := newPeerWatchdog(c.config.Clock, c.config.PeerSilenceTimeout)
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(c.config.MaxMessageBytes),
		grpc.MaxSendMsgSize(c.config.MaxMessageBytes),
		grpc.UnaryInterceptor(watchdog.unaryInterceptor),
		grpc.StreamInterceptor(watchdog.streamInterceptor),
	)
	service := &agentService{handler: handler, info: info, clock: c.config.Clock, maxMessageBytes: c.config.MaxMessageBytes}
	server.RegisterService(&agentControlServiceDesc, service)
	session := &agentSession{sessionCore: newSessionCore(info), conn: conn, listener: one, server: server}
	go func() {
		err := server.Serve(one)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			session.finish(err)
		} else {
			session.finish(nil)
		}
	}()
	go watchdog.run(session, c.config.TickerFactory)
	return session, nil
}

type ServerConfig struct {
	Listener             net.Listener
	TLSConfig            *tls.Config
	Verifier             CredentialVerifier
	Clock                Clock
	Random               io.Reader
	OfflineAfter         time.Duration
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	TickerFactory        TickerFactory
	LivenessObserver     LivenessObserver
	Registry             *SessionRegistry
	MaxCredentialBytes   int
	MaxMessageBytes      int
	ProtectedConcurrency int
	BulkConcurrency      int
	HandshakeTimeout     time.Duration
}

type ServerAcceptor struct {
	config  ServerConfig
	mu      sync.Mutex
	closed  bool
	accepts chan acceptResult
	done    chan struct{}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func NewServerAcceptor(config ServerConfig) (*ServerAcceptor, error) {
	if config.Listener == nil || config.TLSConfig == nil || config.Verifier == nil {
		return nil, errors.New("Server listener, TLS, and credential verifier are required")
	}
	if err := validateProductTLSConfig(config.TLSConfig); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.OfflineAfter == 0 {
		config.OfflineAfter = DefaultOfflineAfter
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if config.HeartbeatTimeout == 0 {
		config.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if config.TickerFactory == nil {
		config.TickerFactory = realTickerFactory{}
	}
	if config.Registry == nil || config.Registry.store == nil {
		return nil, errors.New("Server requires a session registry backed by a durable incarnation watermark store")
	}
	if config.MaxCredentialBytes == 0 {
		config.MaxCredentialBytes = DefaultMaxCredentialBytes
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if config.ProtectedConcurrency == 0 {
		config.ProtectedConcurrency = 8
	}
	if config.BulkConcurrency == 0 {
		config.BulkConcurrency = 32
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if config.OfflineAfter <= 0 || config.HeartbeatInterval <= 0 || config.HeartbeatTimeout <= 0 ||
		config.MaxCredentialBytes <= 0 || config.MaxMessageBytes <= 0 || config.HandshakeTimeout <= 0 {
		return nil, errors.New("Server transport limits must be positive")
	}
	if _, err := NewPriorityGate(config.ProtectedConcurrency, config.BulkConcurrency); err != nil {
		return nil, err
	}
	acceptor := &ServerAcceptor{
		config: config, accepts: make(chan acceptResult), done: make(chan struct{}),
	}
	go acceptor.acceptLoop()
	return acceptor, nil
}

func (a *ServerAcceptor) Accept(ctx context.Context) (ControlSession, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	raw, err := a.accept(ctx)
	if err != nil {
		return nil, err
	}
	conn := tls.Server(raw, productTLSConfig(a.config.TLSConfig))
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, a.config.HandshakeTimeout)
	defer cancelHandshake()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	if conn.ConnectionState().NegotiatedProtocol != productALPN {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: product ALPN was not negotiated", ErrProtocol)
	}
	info, err := serverHandshake(handshakeCtx, conn, a.config.Verifier, a.config.Clock.Now(), a.config.MaxCredentialBytes, a.config.Random)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	info.SourceIP = remoteIP(raw.RemoteAddr())
	dialer := &singleConnDialer{conn: conn}
	client, err := grpc.NewClient("passthrough:///dockpilot-product-agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer.DialContext),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(a.config.MaxMessageBytes),
			grpc.MaxCallSendMsgSize(a.config.MaxMessageBytes),
		),
	)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create Reverse gRPC client: %w", err)
	}
	gate, _ := NewPriorityGate(a.config.ProtectedConcurrency, a.config.BulkConcurrency)
	session := &controlSession{
		sessionCore: newSessionCore(info), client: client, conn: conn,
		clock: a.config.Clock, offlineAfter: a.config.OfflineAfter,
		lastHeartbeat: a.config.Clock.Now(), gate: gate, registry: a.config.Registry,
		maxMessageBytes: a.config.MaxMessageBytes,
	}
	if err := a.config.Registry.RegisterContext(handshakeCtx, session); err != nil {
		_ = client.Close()
		_ = conn.Close()
		session.finish(err)
		return nil, err
	}
	client.Connect()
	go runHeartbeatLoop(session, a.config.HeartbeatInterval, a.config.HeartbeatTimeout, a.config.TickerFactory, a.config.LivenessObserver)
	return session, nil
}

func remoteIP(address net.Addr) string {
	if address == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (a *ServerAcceptor) Registry() *SessionRegistry { return a.config.Registry }

func (a *ServerAcceptor) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	close(a.done)
	a.mu.Unlock()
	return a.config.Listener.Close()
}

func (a *ServerAcceptor) accept(ctx context.Context) (net.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result, ok := <-a.accepts:
		if !ok {
			return nil, net.ErrClosed
		}
		return result.conn, result.err
	}
}

func (a *ServerAcceptor) acceptLoop() {
	defer close(a.accepts)
	for {
		conn, err := a.config.Listener.Accept()
		result := acceptResult{conn: conn, err: err}
		select {
		case a.accepts <- result:
		case <-a.done:
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
		if err != nil {
			return
		}
	}
}

func productTLSConfig(base *tls.Config) *tls.Config {
	config := base.Clone()
	config.NextProtos = []string{productALPN}
	config.MinVersion = tls.VersionTLS13
	return config
}

func validateProductTLSConfig(config *tls.Config) error {
	if config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS13 {
		return errors.New("product transport requires TLS 1.3 or newer")
	}
	return nil
}

type agentSession struct {
	sessionCore
	conn     net.Conn
	listener *singleConnListener
	server   *grpc.Server
}

func (s *agentSession) Close(cause error) error {
	s.server.Stop()
	_ = s.listener.Close()
	s.finish(cause)
	return nil
}

type controlSession struct {
	sessionCore
	client          *grpc.ClientConn
	conn            net.Conn
	clock           Clock
	gate            *PriorityGate
	registry        *SessionRegistry
	closeOnce       sync.Once
	closeErr        error
	maxMessageBytes int

	livenessMu    sync.RWMutex
	lastHeartbeat time.Time
	offlineAfter  time.Duration
}

func (s *controlSession) Heartbeat(ctx context.Context) (Heartbeat, error) {
	var heartbeat Heartbeat
	err := s.Do(ctx, P0Control, func(ctx context.Context) error {
		sentAt := s.clock.Now()
		request := &pb.HeartbeatRequest{SentAtUnixNano: sentAt.UnixNano()}
		var response pb.HeartbeatResponse
		if err := s.client.Invoke(ctx, heartbeatMethod, request, &response); err != nil {
			return err
		}
		heartbeat = Heartbeat{
			SentAt: sentAt, ObservedAt: time.Unix(0, response.GetObservedAtUnixNano()),
			Capability: capabilityFromWire(response.GetCapability()),
		}
		s.livenessMu.Lock()
		s.lastHeartbeat = s.clock.Now()
		s.livenessMu.Unlock()
		return nil
	})
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			_ = s.Close(err)
		}
		return Heartbeat{}, err
	}
	return heartbeat, nil
}

func (s *controlSession) Query(ctx context.Context, request QueryRequest) (QueryResponse, error) {
	var response pb.QueryResponse
	err := s.Do(ctx, P2InteractiveQuery, func(ctx context.Context) error {
		return s.client.Invoke(ctx, queryMethod, &pb.QueryRequest{
			Kind: request.Kind, Target: request.Target, Payload: append([]byte(nil), request.Payload...),
		}, &response)
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return QueryResponse{}, fmt.Errorf("%w: query", ErrHandlerUnavailable)
		}
		return QueryResponse{}, err
	}
	return QueryResponse{Payload: append([]byte(nil), response.GetPayload()...)}, nil
}

func (s *controlSession) StartOperation(ctx context.Context, request OperationRequest) (OperationResponse, error) {
	var response pb.OperationResponse
	err := s.Do(ctx, P0Control, func(ctx context.Context) error {
		return s.client.Invoke(ctx, operationMethod, &pb.OperationRequest{
			OperationId: request.OperationID, Type: request.Type, ProjectKey: request.ProjectKey,
			Target: request.Target, Payload: append([]byte(nil), request.Payload...),
		}, &response)
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return OperationResponse{}, fmt.Errorf("%w: operation", ErrHandlerUnavailable)
		}
		return OperationResponse{}, err
	}
	return operationResponseFromWire(&response), nil
}

func (s *controlSession) GetOperation(ctx context.Context, request GetOperationRequest) (GetOperationResponse, error) {
	if err := validateOperationControlID(request.OperationID); err != nil {
		return GetOperationResponse{}, err
	}
	var response pb.GetOperationResponse
	err := s.Do(ctx, P0Control, func(ctx context.Context) error {
		return s.client.Invoke(ctx, getOperationMethod, &pb.GetOperationRequest{OperationId: request.OperationID}, &response)
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return GetOperationResponse{}, fmt.Errorf("%w: operation recovery", ErrHandlerUnavailable)
		}
		return GetOperationResponse{}, err
	}
	if !response.GetFound() {
		if response.GetOperation() != nil {
			return GetOperationResponse{}, fmt.Errorf("%w: not-found operation lookup included a record", ErrProtocol)
		}
		return GetOperationResponse{Found: false}, nil
	}
	if response.GetOperation() == nil {
		return GetOperationResponse{}, fmt.Errorf("%w: found operation lookup omitted its record", ErrProtocol)
	}
	return GetOperationResponse{Found: true, Operation: operationResponseFromWire(response.GetOperation())}, nil
}

func (s *controlSession) CancelOperation(ctx context.Context, request CancelOperationRequest) (CancelOperationResponse, error) {
	if err := validateOperationControlID(request.OperationID); err != nil {
		return CancelOperationResponse{}, err
	}
	if err := validateCancelReason(request.Reason); err != nil {
		return CancelOperationResponse{}, err
	}
	var response pb.CancelOperationResponse
	err := s.Do(ctx, P0Control, func(ctx context.Context) error {
		return s.client.Invoke(ctx, cancelOperationMethod, &pb.CancelOperationRequest{
			OperationId: request.OperationID, Reason: request.Reason,
		}, &response)
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return CancelOperationResponse{}, fmt.Errorf("%w: operation cancellation", ErrHandlerUnavailable)
		}
		return CancelOperationResponse{}, err
	}
	if !validCancelOutcome(response.GetOutcome()) {
		return CancelOperationResponse{}, fmt.Errorf("%w: invalid cancellation outcome", ErrProtocol)
	}
	if response.GetOutcome() == "NOT_FOUND" {
		if response.GetOperation() != nil {
			return CancelOperationResponse{}, fmt.Errorf("%w: NOT_FOUND cancellation included a record", ErrProtocol)
		}
		return CancelOperationResponse{Outcome: response.GetOutcome()}, nil
	}
	if response.GetOperation() == nil {
		return CancelOperationResponse{}, fmt.Errorf("%w: cancellation outcome omitted its record", ErrProtocol)
	}
	return CancelOperationResponse{Outcome: response.GetOutcome(), Operation: operationResponseFromWire(response.GetOperation())}, nil
}

func (s *controlSession) ListActiveOperations(ctx context.Context, _ ListActiveOperationsRequest) (ListActiveOperationsResponse, error) {
	var response pb.ListActiveOperationsResponse
	err := s.Do(ctx, P0Control, func(ctx context.Context) error {
		return s.client.Invoke(ctx, listActiveOperationsMethod, &pb.ListActiveOperationsRequest{}, &response)
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return ListActiveOperationsResponse{}, fmt.Errorf("%w: active operation recovery", ErrHandlerUnavailable)
		}
		return ListActiveOperationsResponse{}, err
	}
	operations, err := activeOperationsFromWire(response.GetOperations())
	if err != nil {
		return ListActiveOperationsResponse{}, err
	}
	return ListActiveOperationsResponse{Operations: operations}, nil
}

func (s *controlSession) OpenLogs(ctx context.Context, request LogRequest) (LogReceiveStream, error) {
	core, err := s.openReceiveStream(ctx, P3BulkInteractive, logsStreamIndex, logsMethod, &pb.LogStreamRequest{
		ContainerId: request.ContainerID, ProjectUid: request.ProjectUID, Services: append([]string(nil), request.Services...),
		Follow: request.Follow, TailLines: request.TailLines,
		ShowStdout: request.ShowStdout, ShowStderr: request.ShowStderr, Timestamps: request.Timestamps,
		Since: request.Since, Until: request.Until,
	})
	if err != nil {
		return nil, err
	}
	return &logReceiveStream{receiveStreamCore: core}, nil
}

func (s *controlSession) OpenStats(ctx context.Context, request StatsRequest) (StatsReceiveStream, error) {
	core, err := s.openReceiveStream(ctx, P4DisposableLive, statsStreamIndex, statsMethod, &pb.StatsStreamRequest{ContainerId: request.ContainerID})
	if err != nil {
		return nil, err
	}
	return &statsReceiveStream{receiveStreamCore: core}, nil
}

func (s *controlSession) OpenMetricsMatrix(ctx context.Context, _ MetricsMatrixRequest) (MetricsMatrixReceiveStream, error) {
	core, err := s.openReceiveStream(ctx, P4DisposableLive, metricsMatrixStreamIndex, metricsMatrixMethod, &pb.MetricsMatrixRequest{})
	if err != nil {
		return nil, err
	}
	return &metricsMatrixReceiveStream{receiveStreamCore: core}, nil
}

func (s *controlSession) OpenAuditSync(ctx context.Context) (AuditReceiveStream, error) {
	core, err := s.openReceiveStream(ctx, P1DurableSync, auditStreamIndex, auditMethod, nil)
	if err != nil {
		return nil, err
	}
	return &auditReceiveStream{receiveStreamCore: core, maxMessageBytes: s.maxMessageBytes}, nil
}

func (s *controlSession) openReceiveStream(ctx context.Context, class TrafficClass, index int, method string, request any) (*receiveStreamCore, error) {
	select {
	case <-s.Done():
		return nil, errors.New("session is closed")
	default:
	}
	release, err := s.gate.Acquire(ctx, class)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := s.client.NewStream(streamCtx, &agentControlServiceDesc.Streams[index], method)
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	if request != nil {
		if err := stream.SendMsg(request); err != nil {
			cancel()
			release()
			return nil, err
		}
		if err := stream.CloseSend(); err != nil {
			cancel()
			release()
			return nil, err
		}
	}
	core := &receiveStreamCore{stream: stream, cancel: cancel, release: release}
	go func() {
		select {
		case <-streamCtx.Done():
		case <-s.Done():
		}
		core.finish()
	}()
	return core, nil
}

type receiveStreamCore struct {
	stream  grpc.ClientStream
	cancel  context.CancelFunc
	release func()
	once    sync.Once
	recvMu  sync.Mutex
	sendMu  sync.Mutex
}

type auditReceiveStream struct {
	*receiveStreamCore
	maxMessageBytes int
}

func (s *auditReceiveStream) Recv(ctx context.Context) (AuditUpstream, error) {
	var message pb.AuditUpstream
	if err := s.recv(ctx, &message); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return AuditUpstream{}, fmt.Errorf("%w: audit sync", ErrHandlerUnavailable)
		}
		return AuditUpstream{}, err
	}
	return auditUpstreamFromWire(&message)
}

func (s *auditReceiveStream) SendAck(ack AuditAck) error {
	message, err := auditAckToWire(ack)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.SendMsg(message)
}

func (s *receiveStreamCore) recv(ctx context.Context, message any) error {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	type result struct{ err error }
	done := make(chan result, 1)
	go func() { done <- result{err: s.stream.RecvMsg(message)} }()
	select {
	case <-ctx.Done():
		s.finish()
		return ctx.Err()
	case result := <-done:
		if result.err != nil {
			s.finish()
		}
		return result.err
	}
}

func (s *receiveStreamCore) finish() {
	s.once.Do(func() {
		s.cancel()
		s.release()
	})
}

func (s *receiveStreamCore) Close() error {
	s.finish()
	return nil
}

type logReceiveStream struct{ *receiveStreamCore }

func (s *logReceiveStream) Recv(ctx context.Context) (LogEvent, error) {
	var event pb.LogEvent
	if err := s.recv(ctx, &event); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return LogEvent{}, fmt.Errorf("%w: logs", ErrHandlerUnavailable)
		}
		return LogEvent{}, err
	}
	return LogEvent{
		Data: append([]byte(nil), event.GetData()...), Stream: event.GetStream(), LineCount: event.GetLineCount(),
		Timestamp: timeFromUnixNano(event.GetTimestampUnixNano()), DroppedBytes: event.GetDroppedBytes(),
		DroppedLines: event.GetDroppedLines(), Terminal: event.GetTerminal(), Error: event.GetError(),
	}, nil
}

// statsSampleFromWire is shared by the single-container stream and the matrix
// frame so the two cannot drift into reporting the same sample differently.
func statsSampleFromWire(sample *pb.StatsSample) StatsSample {
	return StatsSample{
		ContainerID: sample.GetContainerId(), ObservedAt: timeFromUnixNano(sample.GetObservedAtUnixNano()),
		CPUPercent: sample.GetCpuPercent(), MemoryUsage: sample.GetMemoryUsage(), MemoryLimit: sample.GetMemoryLimit(),
		NetworkRX: sample.GetNetworkRx(), NetworkTX: sample.GetNetworkTx(), BlockRead: sample.GetBlockRead(),
		BlockWrite: sample.GetBlockWrite(), RestartCount: sample.GetRestartCount(), Health: sample.GetHealth(),
		Uptime: time.Duration(sample.GetUptimeNano()),
	}
}

type metricsMatrixReceiveStream struct{ *receiveStreamCore }

func (s *metricsMatrixReceiveStream) Recv(ctx context.Context) (MetricsMatrixFrame, error) {
	var frame pb.MetricsMatrixFrame
	if err := s.recv(ctx, &frame); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return MetricsMatrixFrame{}, fmt.Errorf("%w: metrics matrix", ErrHandlerUnavailable)
		}
		return MetricsMatrixFrame{}, err
	}
	return metricsMatrixFrameFromWire(&frame), nil
}

func metricsMatrixFrameFromWire(frame *pb.MetricsMatrixFrame) MetricsMatrixFrame {
	result := MetricsMatrixFrame{
		ObservedAt:          timeFromUnixNano(frame.GetObservedAtUnixNano()),
		DroppedFrames:       frame.GetDroppedFrames(),
		PendingContainerIDs: append([]string(nil), frame.GetPendingContainerIds()...),
		MembershipStale:     frame.GetMembershipStale(),
		MembershipReason:    frame.GetMembershipReason(),
		WorkloadStale:       frame.GetWorkloadStale(),
		WorkloadReason:      frame.GetWorkloadReason(),
	}
	if workload := frame.GetWorkload(); workload != nil {
		result.Workload = WorkloadSummary{
			CPUCapacity: workload.GetCpuCapacity(), MemoryCapacity: workload.GetMemoryCapacity(),
			ContainersRunning: workload.GetContainersRunning(), ContainersTotal: workload.GetContainersTotal(),
		}
		for _, filesystem := range workload.GetFilesystems() {
			result.Workload.Filesystems = append(result.Workload.Filesystems, ManagedFilesystem{
				Path: filesystem.GetPath(), TotalBytes: filesystem.GetTotalBytes(), FreeBytes: filesystem.GetFreeBytes(),
				Unavailable: filesystem.GetUnavailable(), Reason: filesystem.GetReason(),
			})
		}
	}
	for _, sample := range frame.GetContainers() {
		result.Containers = append(result.Containers, statsSampleFromWire(sample))
	}
	return result
}

type statsReceiveStream struct{ *receiveStreamCore }

func (s *statsReceiveStream) Recv(ctx context.Context) (StatsSample, error) {
	var sample pb.StatsSample
	if err := s.recv(ctx, &sample); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return StatsSample{}, fmt.Errorf("%w: stats", ErrHandlerUnavailable)
		}
		return StatsSample{}, err
	}
	return statsSampleFromWire(&sample), nil
}

func (s *controlSession) Do(ctx context.Context, class TrafficClass, work func(context.Context) error) error {
	if work == nil {
		return errors.New("transport work is required")
	}
	select {
	case <-s.Done():
		return errors.New("session is closed")
	default:
	}
	release, err := s.gate.Acquire(ctx, class)
	if err != nil {
		return err
	}
	defer release()
	return work(ctx)
}

func (s *controlSession) State() State {
	select {
	case <-s.Done():
		return StateClosed
	default:
	}
	if s.clock.Now().Sub(s.LastHeartbeat()) >= s.offlineAfter {
		return StateOffline
	}
	return StateActive
}

func (s *controlSession) LastHeartbeat() time.Time {
	s.livenessMu.RLock()
	defer s.livenessMu.RUnlock()
	return s.lastHeartbeat
}

func (s *controlSession) Close(cause error) error {
	s.closeOnce.Do(func() {
		s.finish(cause)
		if s.client != nil {
			s.closeErr = s.client.Close()
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.registry != nil {
			s.registry.SessionClosed(s.info.AgentID, s.info.SessionID)
		}
	})
	return s.closeErr
}

type agentServiceAPI interface {
	heartbeat(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)
	query(context.Context, *pb.QueryRequest) (*pb.QueryResponse, error)
	startOperation(context.Context, *pb.OperationRequest) (*pb.OperationResponse, error)
	getOperation(context.Context, *pb.GetOperationRequest) (*pb.GetOperationResponse, error)
	cancelOperation(context.Context, *pb.CancelOperationRequest) (*pb.CancelOperationResponse, error)
	listActiveOperations(context.Context, *pb.ListActiveOperationsRequest) (*pb.ListActiveOperationsResponse, error)
	syncAudit(grpc.ServerStream) error
	streamLogs(*pb.LogStreamRequest, grpc.ServerStream) error
	streamStats(*pb.StatsStreamRequest, grpc.ServerStream) error
	streamMetricsMatrix(*pb.MetricsMatrixRequest, grpc.ServerStream) error
}

type grpcAuditSyncStream struct {
	stream          grpc.ServerStream
	maxMessageBytes int
	sendMu          sync.Mutex
	recvMu          sync.Mutex
}

func (s *grpcAuditSyncStream) Context() context.Context { return s.stream.Context() }

func (s *grpcAuditSyncStream) Send(message AuditUpstream) error {
	wire, err := auditUpstreamToWire(message, s.maxMessageBytes)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.SendMsg(wire)
}

func (s *grpcAuditSyncStream) ReceiveAck() (AuditAck, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	var message pb.AuditAck
	if err := s.stream.RecvMsg(&message); err != nil {
		return AuditAck{}, err
	}
	return auditAckFromWire(&message)
}

func (s *agentService) syncAudit(stream grpc.ServerStream) error {
	handler, ok := s.handler.(AuditSyncHandler)
	if !ok {
		return status.Error(codes.Unimplemented, "Agent audit sync handler is not configured")
	}
	err := handler.SyncAudit(stream.Context(), s.info, &grpcAuditSyncStream{
		stream: stream, maxMessageBytes: s.maxMessageBytes,
	})
	if errors.Is(err, ErrHandlerUnavailable) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

func (s *agentService) query(ctx context.Context, request *pb.QueryRequest) (*pb.QueryResponse, error) {
	handler, ok := s.handler.(QueryHandler)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Agent query handler is not configured")
	}
	response, err := handler.Query(ctx, s.info, QueryRequest{
		Kind: request.GetKind(), Target: request.GetTarget(), Payload: append([]byte(nil), request.GetPayload()...),
	})
	if err != nil {
		return nil, err
	}
	return &pb.QueryResponse{Payload: append([]byte(nil), response.Payload...)}, nil
}

func (s *agentService) startOperation(ctx context.Context, request *pb.OperationRequest) (*pb.OperationResponse, error) {
	handler, ok := s.handler.(OperationHandler)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Agent operation handler is not configured")
	}
	response, err := handler.StartOperation(ctx, s.info, OperationRequest{
		OperationID: request.GetOperationId(), Type: request.GetType(), ProjectKey: request.GetProjectKey(),
		Target: request.GetTarget(), Payload: append([]byte(nil), request.GetPayload()...),
	})
	if err != nil {
		return nil, err
	}
	return operationResponseToWire(response), nil
}

func (s *agentService) getOperation(ctx context.Context, request *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
	if err := validateOperationControlID(request.GetOperationId()); err != nil {
		return nil, err
	}
	handler, ok := s.handler.(OperationControlHandler)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Agent operation recovery handler is not configured")
	}
	response, err := handler.GetOperation(ctx, s.info, GetOperationRequest{OperationID: request.GetOperationId()})
	if err != nil {
		return nil, err
	}
	if !response.Found {
		return &pb.GetOperationResponse{}, nil
	}
	return &pb.GetOperationResponse{Found: true, Operation: operationResponseToWire(response.Operation)}, nil
}

func (s *agentService) cancelOperation(ctx context.Context, request *pb.CancelOperationRequest) (*pb.CancelOperationResponse, error) {
	if err := validateOperationControlID(request.GetOperationId()); err != nil {
		return nil, err
	}
	if err := validateCancelReason(request.GetReason()); err != nil {
		return nil, err
	}
	handler, ok := s.handler.(OperationControlHandler)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Agent operation cancellation handler is not configured")
	}
	response, err := handler.CancelOperation(ctx, s.info, CancelOperationRequest{
		OperationID: request.GetOperationId(), Reason: request.GetReason(),
	})
	if err != nil {
		return nil, err
	}
	if !validCancelOutcome(response.Outcome) {
		return nil, status.Error(codes.Internal, "Agent returned an invalid cancellation outcome")
	}
	wire := &pb.CancelOperationResponse{Outcome: response.Outcome}
	if response.Outcome != "NOT_FOUND" {
		wire.Operation = operationResponseToWire(response.Operation)
	}
	return wire, nil
}

func (s *agentService) listActiveOperations(ctx context.Context, _ *pb.ListActiveOperationsRequest) (*pb.ListActiveOperationsResponse, error) {
	handler, ok := s.handler.(OperationRecoveryHandler)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Agent active operation recovery handler is not configured")
	}
	response, err := handler.ListActiveOperations(ctx, s.info, ListActiveOperationsRequest{})
	if err != nil {
		if errors.Is(err, ErrHandlerUnavailable) {
			return nil, status.Error(codes.Unimplemented, err.Error())
		}
		return nil, err
	}
	if len(response.Operations) > MaxActiveOperationCount {
		return nil, status.Error(codes.ResourceExhausted, "active operation count exceeds protocol limit")
	}
	wire := &pb.ListActiveOperationsResponse{Operations: make([]*pb.ActiveOperation, 0, len(response.Operations))}
	previousID := ""
	for index, active := range response.Operations {
		if active.OperationID == "" || len(active.OperationID) > MaxOperationIDBytes || !utf8.ValidString(active.OperationID) {
			return nil, status.Error(codes.Internal, "Agent returned an invalid active operation ID")
		}
		if index > 0 && active.OperationID <= previousID {
			return nil, status.Error(codes.Internal, "Agent returned non-deterministically ordered active operations")
		}
		if len(active.Operation.OutputTail) > MaxOperationOutputTailBytes {
			return nil, status.Error(codes.ResourceExhausted, "active operation output tail exceeds protocol limit")
		}
		wire.Operations = append(wire.Operations, &pb.ActiveOperation{
			OperationId: active.OperationID, Type: active.Type, ProjectKey: active.ProjectKey,
			Target: active.Target, Operation: operationResponseToWire(active.Operation),
		})
		previousID = active.OperationID
	}
	if proto.Size(wire) > s.maxMessageBytes {
		return nil, status.Error(codes.ResourceExhausted, "active operation response exceeds transport message limit")
	}
	return wire, nil
}

func activeOperationsFromWire(wire []*pb.ActiveOperation) ([]ActiveOperation, error) {
	if len(wire) > MaxActiveOperationCount {
		return nil, fmt.Errorf("%w: active operation count exceeds protocol limit", ErrProtocol)
	}
	operations := make([]ActiveOperation, 0, len(wire))
	previousID := ""
	for index, active := range wire {
		if active == nil || active.GetOperationId() == "" || len(active.GetOperationId()) > MaxOperationIDBytes ||
			!utf8.ValidString(active.GetOperationId()) || active.GetOperation() == nil {
			return nil, fmt.Errorf("%w: invalid active operation record", ErrProtocol)
		}
		if index > 0 && active.GetOperationId() <= previousID {
			return nil, fmt.Errorf("%w: active operations are not strictly ordered", ErrProtocol)
		}
		if len(active.GetOperation().GetOutputTail()) > MaxOperationOutputTailBytes {
			return nil, fmt.Errorf("%w: active operation output tail exceeds protocol limit", ErrProtocol)
		}
		operations = append(operations, ActiveOperation{
			OperationID: active.GetOperationId(), Type: active.GetType(), ProjectKey: active.GetProjectKey(),
			Target: active.GetTarget(), Operation: operationResponseFromWire(active.GetOperation()),
		})
		previousID = active.GetOperationId()
	}
	return operations, nil
}

func operationResponseToWire(response OperationResponse) *pb.OperationResponse {
	return &pb.OperationResponse{
		Status: response.Status, Phase: response.Phase, Revision: response.Revision,
		PartialEffectsPossible: response.PartialEffectsPossible, Error: response.Error,
		OutputTail: append([]byte(nil), response.OutputTail...), OutputTruncated: response.OutputTruncated,
		CancelMode: response.CancelMode, CanCancel: response.CanCancel, CancelabilityReason: response.CancelabilityReason,
		RequestedAtUnixNano: timeToUnixNano(response.RequestedAt), StartedAtUnixNano: timeToUnixNano(response.StartedAt),
		FinishedAtUnixNano: timeToUnixNano(response.FinishedAt),
	}
}

func operationResponseFromWire(response *pb.OperationResponse) OperationResponse {
	if response == nil {
		return OperationResponse{}
	}
	return OperationResponse{
		Status: response.GetStatus(), Phase: response.GetPhase(), Revision: response.GetRevision(),
		PartialEffectsPossible: response.GetPartialEffectsPossible(), Error: response.GetError(),
		OutputTail: append([]byte(nil), response.GetOutputTail()...), OutputTruncated: response.GetOutputTruncated(),
		CancelMode: response.GetCancelMode(), CanCancel: response.GetCanCancel(), CancelabilityReason: response.GetCancelabilityReason(),
		RequestedAt: unixNanoToTime(response.GetRequestedAtUnixNano()), StartedAt: unixNanoToTime(response.GetStartedAtUnixNano()),
		FinishedAt: unixNanoToTime(response.GetFinishedAtUnixNano()),
	}
}

func timeToUnixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func unixNanoToTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func validateOperationControlID(operationID string) error {
	if operationID == "" || len(operationID) > MaxOperationIDBytes || !utf8.ValidString(operationID) {
		return status.Errorf(codes.InvalidArgument, "operation_id must be valid UTF-8 between 1 and %d bytes", MaxOperationIDBytes)
	}
	return nil
}

func validateCancelReason(reason string) error {
	if reason == "" || len(reason) > MaxCancelReasonBytes || !utf8.ValidString(reason) {
		return status.Errorf(codes.InvalidArgument, "cancel reason must be valid UTF-8 between 1 and %d bytes", MaxCancelReasonBytes)
	}
	switch reason {
	case "USER", "TIMEOUT", "AGENT_SHUTDOWN":
		return nil
	default:
		return status.Error(codes.InvalidArgument, "cancel reason is not supported")
	}
}

func validCancelOutcome(outcome string) bool {
	switch outcome {
	case "ACCEPTED", "TOO_LATE", "NOT_CANCELABLE", "ALREADY_TERMINAL", "NOT_FOUND":
		return true
	default:
		return false
	}
}

type agentService struct {
	handler         AgentHandler
	info            SessionInfo
	clock           Clock
	maxMessageBytes int
}

type grpcLogSender struct {
	stream          grpc.ServerStream
	maxMessageBytes int
}

func (s grpcLogSender) Send(event LogEvent) error {
	if len(event.Data) > s.maxMessageBytes {
		return status.Error(codes.ResourceExhausted, "log event exceeds transport message limit")
	}
	return s.stream.SendMsg(&pb.LogEvent{
		Data: append([]byte(nil), event.Data...), Stream: event.Stream, LineCount: event.LineCount,
		TimestampUnixNano: unixNanoOrZero(event.Timestamp), DroppedBytes: event.DroppedBytes,
		DroppedLines: event.DroppedLines, Terminal: event.Terminal, Error: event.Error,
	})
}

func (s *agentService) streamLogs(request *pb.LogStreamRequest, stream grpc.ServerStream) error {
	handler, ok := s.handler.(LogStreamHandler)
	if !ok {
		return status.Error(codes.Unimplemented, "Agent log stream handler is not configured")
	}
	err := handler.StreamLogs(stream.Context(), s.info, LogRequest{
		ContainerID: request.GetContainerId(), ProjectUID: request.GetProjectUid(), Services: append([]string(nil), request.GetServices()...),
		Follow: request.GetFollow(), TailLines: request.GetTailLines(),
		ShowStdout: request.GetShowStdout(), ShowStderr: request.GetShowStderr(), Timestamps: request.GetTimestamps(),
		Since: request.GetSince(), Until: request.GetUntil(),
	}, grpcLogSender{stream: stream, maxMessageBytes: s.maxMessageBytes})
	if errors.Is(err, ErrHandlerUnavailable) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

type grpcStatsSender struct{ stream grpc.ServerStream }

func (s grpcStatsSender) Send(sample StatsSample) error {
	return s.stream.SendMsg(&pb.StatsSample{
		ContainerId: sample.ContainerID, ObservedAtUnixNano: unixNanoOrZero(sample.ObservedAt),
		CpuPercent: sample.CPUPercent, MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit,
		NetworkRx: sample.NetworkRX, NetworkTx: sample.NetworkTX, BlockRead: sample.BlockRead,
		BlockWrite: sample.BlockWrite, RestartCount: sample.RestartCount, Health: sample.Health,
		UptimeNano: int64(sample.Uptime),
	})
}

type grpcMetricsMatrixSender struct{ stream grpc.ServerStream }

func (s grpcMetricsMatrixSender) Send(frame MetricsMatrixFrame) error {
	wire := &pb.MetricsMatrixFrame{
		ObservedAtUnixNano:  unixNanoOrZero(frame.ObservedAt),
		DroppedFrames:       frame.DroppedFrames,
		PendingContainerIds: append([]string(nil), frame.PendingContainerIDs...),
		MembershipStale:     frame.MembershipStale,
		MembershipReason:    frame.MembershipReason,
		WorkloadStale:       frame.WorkloadStale,
		WorkloadReason:      frame.WorkloadReason,
		Workload: &pb.WorkloadSummary{
			CpuCapacity: frame.Workload.CPUCapacity, MemoryCapacity: frame.Workload.MemoryCapacity,
			ContainersRunning: frame.Workload.ContainersRunning, ContainersTotal: frame.Workload.ContainersTotal,
		},
	}
	for _, filesystem := range frame.Workload.Filesystems {
		wire.Workload.Filesystems = append(wire.Workload.Filesystems, &pb.ManagedFilesystem{
			Path: filesystem.Path, TotalBytes: filesystem.TotalBytes, FreeBytes: filesystem.FreeBytes,
			Unavailable: filesystem.Unavailable, Reason: filesystem.Reason,
		})
	}
	for _, sample := range frame.Containers {
		wire.Containers = append(wire.Containers, &pb.StatsSample{
			ContainerId: sample.ContainerID, ObservedAtUnixNano: unixNanoOrZero(sample.ObservedAt),
			CpuPercent: sample.CPUPercent, MemoryUsage: sample.MemoryUsage, MemoryLimit: sample.MemoryLimit,
			NetworkRx: sample.NetworkRX, NetworkTx: sample.NetworkTX, BlockRead: sample.BlockRead,
			BlockWrite: sample.BlockWrite, RestartCount: sample.RestartCount, Health: sample.Health,
			UptimeNano: int64(sample.Uptime),
		})
	}
	return s.stream.SendMsg(wire)
}

func (s *agentService) streamMetricsMatrix(_ *pb.MetricsMatrixRequest, stream grpc.ServerStream) error {
	handler, ok := s.handler.(MetricsMatrixStreamHandler)
	if !ok {
		return status.Error(codes.Unimplemented, "Agent metrics matrix stream handler is not configured")
	}
	err := handler.StreamMetricsMatrix(stream.Context(), s.info, MetricsMatrixRequest{}, grpcMetricsMatrixSender{stream: stream})
	if errors.Is(err, ErrHandlerUnavailable) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

func (s *agentService) streamStats(request *pb.StatsStreamRequest, stream grpc.ServerStream) error {
	handler, ok := s.handler.(StatsStreamHandler)
	if !ok {
		return status.Error(codes.Unimplemented, "Agent stats stream handler is not configured")
	}
	err := handler.StreamStats(stream.Context(), s.info, StatsRequest{ContainerID: request.GetContainerId()}, grpcStatsSender{stream: stream})
	if errors.Is(err, ErrHandlerUnavailable) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

func auditUpstreamToWire(message AuditUpstream, maxMessageBytes int) (*pb.AuditUpstream, error) {
	count := 0
	if message.Record != nil {
		count++
	}
	if message.Coverage != nil {
		count++
	}
	if message.CursorBehindFloor != nil {
		count++
	}
	if message.AckResult != nil {
		count++
	}
	if count != 1 {
		return nil, fmt.Errorf("%w: audit upstream requires exactly one message", ErrProtocol)
	}
	if record := message.Record; record != nil {
		if record.Incarnation == 0 || record.Sequence == 0 || len(record.Payload) > maxMessageBytes {
			return nil, fmt.Errorf("%w: invalid audit record", ErrProtocol)
		}
		return &pb.AuditUpstream{Message: &pb.AuditUpstream_Record{Record: &pb.AuditRecord{
			Cursor:             &pb.AuditCursor{Incarnation: record.Incarnation, Sequence: record.Sequence},
			AppendedAtUnixNano: unixNanoOrZero(record.AppendedAt), Payload: append([]byte(nil), record.Payload...),
		}}}, nil
	}
	if coverage := message.Coverage; coverage != nil {
		wire, err := auditCoverageToWire(*coverage)
		if err != nil {
			return nil, err
		}
		return &pb.AuditUpstream{Message: &pb.AuditUpstream_Coverage{Coverage: wire}}, nil
	}
	if behind := message.CursorBehindFloor; behind != nil {
		wire, err := auditBehindFloorToWire(*behind)
		if err != nil {
			return nil, err
		}
		return &pb.AuditUpstream{Message: &pb.AuditUpstream_CursorBehindFloor{CursorBehindFloor: wire}}, nil
	}
	result, err := auditAckResultToWire(*message.AckResult)
	if err != nil {
		return nil, err
	}
	return &pb.AuditUpstream{Message: &pb.AuditUpstream_AckResult{AckResult: result}}, nil
}

func auditUpstreamFromWire(message *pb.AuditUpstream) (AuditUpstream, error) {
	if message == nil {
		return AuditUpstream{}, fmt.Errorf("%w: nil audit upstream", ErrProtocol)
	}
	switch body := message.GetMessage().(type) {
	case *pb.AuditUpstream_Record:
		if body.Record == nil || !validAuditCursorWire(body.Record.GetCursor()) {
			return AuditUpstream{}, fmt.Errorf("%w: invalid audit record", ErrProtocol)
		}
		return AuditUpstream{Record: &AuditRecord{
			Incarnation: body.Record.GetCursor().GetIncarnation(), Sequence: body.Record.GetCursor().GetSequence(),
			AppendedAt: timeFromUnixNano(body.Record.GetAppendedAtUnixNano()), Payload: append([]byte(nil), body.Record.GetPayload()...),
		}}, nil
	case *pb.AuditUpstream_Coverage:
		coverage, err := auditCoverageFromWire(body.Coverage)
		if err != nil {
			return AuditUpstream{}, err
		}
		return AuditUpstream{Coverage: &coverage}, nil
	case *pb.AuditUpstream_CursorBehindFloor:
		behind, err := auditBehindFloorFromWire(body.CursorBehindFloor)
		if err != nil {
			return AuditUpstream{}, err
		}
		return AuditUpstream{CursorBehindFloor: &behind}, nil
	case *pb.AuditUpstream_AckResult:
		result, err := auditAckResultFromWire(body.AckResult)
		if err != nil {
			return AuditUpstream{}, err
		}
		return AuditUpstream{AckResult: &result}, nil
	default:
		return AuditUpstream{}, fmt.Errorf("%w: audit upstream body is missing", ErrProtocol)
	}
}

func auditAckResultToWire(result AuditAckResult) (*pb.AuditAckResult, error) {
	if !validAuditCursor(result.Proposed) || (result.Accepted && (result.StaleCoverage != nil || result.Error != "")) ||
		(!result.Accepted && (result.StaleCoverage == nil || result.Error == "")) {
		return nil, fmt.Errorf("%w: invalid audit ACK result", ErrProtocol)
	}
	wire := &pb.AuditAckResult{Proposed: auditCursorToWire(result.Proposed), Accepted: result.Accepted, Error: result.Error}
	if result.StaleCoverage != nil {
		coverage, err := auditCoverageToWire(*result.StaleCoverage)
		if err != nil {
			return nil, err
		}
		wire.StaleCoverage = coverage
	}
	return wire, nil
}

func auditAckResultFromWire(wire *pb.AuditAckResult) (AuditAckResult, error) {
	if wire == nil || !validAuditCursorWire(wire.GetProposed()) ||
		(wire.GetAccepted() && (wire.GetStaleCoverage() != nil || wire.GetError() != "")) ||
		(!wire.GetAccepted() && (wire.GetStaleCoverage() == nil || wire.GetError() == "")) {
		return AuditAckResult{}, fmt.Errorf("%w: invalid audit ACK result", ErrProtocol)
	}
	result := AuditAckResult{Proposed: auditCursorFromWire(wire.GetProposed()), Accepted: wire.GetAccepted(), Error: wire.GetError()}
	if wire.GetStaleCoverage() != nil {
		coverage, err := auditCoverageFromWire(wire.GetStaleCoverage())
		if err != nil {
			return AuditAckResult{}, err
		}
		result.StaleCoverage = &coverage
	}
	return result, nil
}

func auditAckToWire(ack AuditAck) (*pb.AuditAck, error) {
	if ack.IsArchiveAnnouncement() {
		archive := ack.Archive
		if archive.ServerIdentityID == "" || archive.Generation == 0 || archive.AuditArchiveID == "" {
			return nil, fmt.Errorf("%w: invalid audit archive descriptor", ErrProtocol)
		}
		return &pb.AuditAck{AuditArchiveId: archive.AuditArchiveID, Archive: &pb.AuditArchiveDescriptor{
			ServerIdentityId: archive.ServerIdentityID, Generation: archive.Generation,
			AuditArchiveId: archive.AuditArchiveID,
		}}, nil
	}
	if ack.AuditArchiveID == "" || ack.Incarnation == 0 || ack.Sequence == 0 {
		return nil, fmt.Errorf("%w: invalid audit ACK", ErrProtocol)
	}
	return &pb.AuditAck{AuditArchiveId: ack.AuditArchiveID,
		Cursor:               &pb.AuditCursor{Incarnation: ack.Incarnation, Sequence: ack.Sequence},
		CoverageRevisionSeen: ack.CoverageRevisionSeen}, nil
}

func auditAckFromWire(message *pb.AuditAck) (AuditAck, error) {
	if message == nil || message.GetAuditArchiveId() == "" {
		return AuditAck{}, fmt.Errorf("%w: invalid audit ACK", ErrProtocol)
	}
	if wire := message.GetArchive(); wire != nil && message.GetCursor() == nil {
		if wire.GetServerIdentityId() == "" || wire.GetGeneration() == 0 || wire.GetAuditArchiveId() == "" {
			return AuditAck{}, fmt.Errorf("%w: invalid audit archive descriptor", ErrProtocol)
		}
		return AuditAck{AuditArchiveID: message.GetAuditArchiveId(), Archive: &AuditArchiveDescriptor{
			ServerIdentityID: wire.GetServerIdentityId(), Generation: wire.GetGeneration(),
			AuditArchiveID: wire.GetAuditArchiveId(),
		}}, nil
	}
	if !validAuditCursorWire(message.GetCursor()) {
		return AuditAck{}, fmt.Errorf("%w: invalid audit ACK", ErrProtocol)
	}
	return AuditAck{AuditArchiveID: message.GetAuditArchiveId(), Incarnation: message.GetCursor().GetIncarnation(),
		Sequence: message.GetCursor().GetSequence(), CoverageRevisionSeen: message.GetCoverageRevisionSeen()}, nil
}

func auditCoverageToWire(coverage AuditCoverageSnapshot) (*pb.AuditCoverageSnapshot, error) {
	wire := &pb.AuditCoverageSnapshot{Revision: coverage.Revision, GeneratedAtUnixNano: unixNanoOrZero(coverage.GeneratedAt),
		CoverageUnknownIncarnations: append([]uint64(nil), coverage.CoverageUnknownIncarnations...)}
	for _, gap := range coverage.Gaps {
		if gap.Incarnation == 0 || gap.FromSequence == 0 || gap.UntilSequence <= gap.FromSequence || gap.Reason == "" || gap.Precision == "" {
			return nil, fmt.Errorf("%w: invalid audit coverage gap", ErrProtocol)
		}
		wire.Gaps = append(wire.Gaps, &pb.AuditGap{Incarnation: gap.Incarnation, FromSequence: gap.FromSequence,
			UntilSequence: gap.UntilSequence, Reason: gap.Reason, Precision: gap.Precision, LastLossRevision: gap.LastLossRevision})
	}
	for _, incarnation := range coverage.CoverageUnknownIncarnations {
		if incarnation == 0 {
			return nil, fmt.Errorf("%w: invalid unknown audit incarnation", ErrProtocol)
		}
	}
	return wire, nil
}

func auditCoverageFromWire(wire *pb.AuditCoverageSnapshot) (AuditCoverageSnapshot, error) {
	if wire == nil {
		return AuditCoverageSnapshot{}, fmt.Errorf("%w: nil audit coverage", ErrProtocol)
	}
	coverage := AuditCoverageSnapshot{Revision: wire.GetRevision(), GeneratedAt: timeFromUnixNano(wire.GetGeneratedAtUnixNano()),
		CoverageUnknownIncarnations: append([]uint64(nil), wire.GetCoverageUnknownIncarnations()...)}
	for _, gap := range wire.GetGaps() {
		if gap == nil || gap.GetIncarnation() == 0 || gap.GetFromSequence() == 0 || gap.GetUntilSequence() <= gap.GetFromSequence() || gap.GetReason() == "" || gap.GetPrecision() == "" {
			return AuditCoverageSnapshot{}, fmt.Errorf("%w: invalid audit coverage gap", ErrProtocol)
		}
		coverage.Gaps = append(coverage.Gaps, AuditGap{Incarnation: gap.GetIncarnation(), FromSequence: gap.GetFromSequence(),
			UntilSequence: gap.GetUntilSequence(), Reason: gap.GetReason(), Precision: gap.GetPrecision(), LastLossRevision: gap.GetLastLossRevision()})
	}
	for _, incarnation := range coverage.CoverageUnknownIncarnations {
		if incarnation == 0 {
			return AuditCoverageSnapshot{}, fmt.Errorf("%w: invalid unknown audit incarnation", ErrProtocol)
		}
	}
	return coverage, nil
}

func auditBehindFloorToWire(behind AuditCursorBehindFloor) (*pb.AuditCursorBehindFloor, error) {
	if !validAuditCursor(behind.Requested) || !validAuditCursor(behind.Bounds.NextCursor) {
		return nil, fmt.Errorf("%w: invalid behind-floor cursors", ErrProtocol)
	}
	coverage, err := auditCoverageToWire(behind.Coverage)
	if err != nil {
		return nil, err
	}
	return &pb.AuditCursorBehindFloor{Requested: auditCursorToWire(behind.Requested),
		Bounds: auditBoundsToWire(behind.Bounds), Coverage: coverage}, nil
}

func auditBehindFloorFromWire(wire *pb.AuditCursorBehindFloor) (AuditCursorBehindFloor, error) {
	if wire == nil || !validAuditCursorWire(wire.GetRequested()) || wire.GetBounds() == nil || !validAuditCursorWire(wire.GetBounds().GetNextCursor()) {
		return AuditCursorBehindFloor{}, fmt.Errorf("%w: invalid behind-floor response", ErrProtocol)
	}
	coverage, err := auditCoverageFromWire(wire.GetCoverage())
	if err != nil {
		return AuditCursorBehindFloor{}, err
	}
	return AuditCursorBehindFloor{Requested: auditCursorFromWire(wire.GetRequested()),
		Bounds: auditBoundsFromWire(wire.GetBounds()), Coverage: coverage}, nil
}

func auditBoundsToWire(bounds AuditBounds) *pb.AuditBounds {
	return &pb.AuditBounds{WalFloor: optionalAuditCursorToWire(bounds.WALFloor), WalCeiling: optionalAuditCursorToWire(bounds.WALCeiling),
		NextCursor: auditCursorToWire(bounds.NextCursor), ServerAckedThrough: optionalAuditCursorToWire(bounds.ServerACKedThrough),
		AcknowledgedArchiveId: bounds.AcknowledgedArchiveID, CoverageRevision: bounds.CoverageRevision}
}

func auditBoundsFromWire(wire *pb.AuditBounds) AuditBounds {
	return AuditBounds{WALFloor: optionalAuditCursorFromWire(wire.GetWalFloor()), WALCeiling: optionalAuditCursorFromWire(wire.GetWalCeiling()),
		NextCursor: auditCursorFromWire(wire.GetNextCursor()), ServerACKedThrough: optionalAuditCursorFromWire(wire.GetServerAckedThrough()),
		AcknowledgedArchiveID: wire.GetAcknowledgedArchiveId(), CoverageRevision: wire.GetCoverageRevision()}
}

func validAuditCursor(cursor AuditCursor) bool {
	return cursor.Incarnation != 0 && cursor.Sequence != 0
}
func validAuditCursorWire(cursor *pb.AuditCursor) bool {
	return cursor != nil && cursor.GetIncarnation() != 0 && cursor.GetSequence() != 0
}
func auditCursorToWire(cursor AuditCursor) *pb.AuditCursor {
	return &pb.AuditCursor{Incarnation: cursor.Incarnation, Sequence: cursor.Sequence}
}
func auditCursorFromWire(cursor *pb.AuditCursor) AuditCursor {
	return AuditCursor{Incarnation: cursor.GetIncarnation(), Sequence: cursor.GetSequence()}
}
func optionalAuditCursorToWire(cursor *AuditCursor) *pb.AuditCursor {
	if cursor == nil {
		return nil
	}
	return auditCursorToWire(*cursor)
}
func optionalAuditCursorFromWire(cursor *pb.AuditCursor) *AuditCursor {
	if cursor == nil {
		return nil
	}
	value := auditCursorFromWire(cursor)
	return &value
}

func unixNanoOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func timeFromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}

func (s *agentService) heartbeat(ctx context.Context, request *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	capability, err := s.handler.Heartbeat(ctx, s.info, time.Unix(0, request.GetSentAtUnixNano()))
	if err != nil {
		return nil, err
	}
	return &pb.HeartbeatResponse{
		ObservedAtUnixNano: s.clock.Now().UnixNano(),
		Capability:         capabilityToWire(capability),
	}, nil
}

func capabilityToWire(capability Capability) *pb.Capability {
	return &pb.Capability{
		ConnectionReady: capability.ConnectionReady, DockerReady: capability.DockerReady,
		ComposeReady: capability.ComposeReady, DockerApiVersion: capability.DockerAPIVersion,
		BundledComposeVersion: capability.BundledComposeVersion, Reason: capability.Reason,
		FsRead: capability.FSRead, FsWrite: capability.FSWrite,
		FsReadReason: capability.FSReadReason, FsWriteReason: capability.FSWriteReason,
		MetricsMatrix: capability.MetricsMatrix,
	}
}

func capabilityFromWire(capability *pb.Capability) Capability {
	if capability == nil {
		return Capability{}
	}
	return Capability{
		ConnectionReady: capability.GetConnectionReady(), DockerReady: capability.GetDockerReady(),
		ComposeReady: capability.GetComposeReady(), DockerAPIVersion: capability.GetDockerApiVersion(),
		BundledComposeVersion: capability.GetBundledComposeVersion(), Reason: capability.GetReason(),
		FSRead: capability.GetFsRead(), FSWrite: capability.GetFsWrite(),
		FSReadReason: capability.GetFsReadReason(), FSWriteReason: capability.GetFsWriteReason(),
		MetricsMatrix: capability.GetMetricsMatrix(),
	}
}

func heartbeatRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.HeartbeatRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).heartbeat(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: heartbeatMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).heartbeat(ctx, request.(*pb.HeartbeatRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func queryRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.QueryRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).query(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: queryMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).query(ctx, request.(*pb.QueryRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func operationRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.OperationRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).startOperation(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: operationMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).startOperation(ctx, request.(*pb.OperationRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func getOperationRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.GetOperationRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).getOperation(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: getOperationMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).getOperation(ctx, request.(*pb.GetOperationRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func cancelOperationRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.CancelOperationRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).cancelOperation(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: cancelOperationMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).cancelOperation(ctx, request.(*pb.CancelOperationRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func listActiveOperationsRPCHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(pb.ListActiveOperationsRequest)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(agentServiceAPI).listActiveOperations(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: listActiveOperationsMethod}
	handler := func(ctx context.Context, request any) (any, error) {
		return service.(agentServiceAPI).listActiveOperations(ctx, request.(*pb.ListActiveOperationsRequest))
	}
	return interceptor(ctx, request, info, handler)
}

func logsRPCHandler(service any, stream grpc.ServerStream) error {
	var request pb.LogStreamRequest
	if err := stream.RecvMsg(&request); err != nil {
		return err
	}
	return service.(agentServiceAPI).streamLogs(&request, stream)
}

func statsRPCHandler(service any, stream grpc.ServerStream) error {
	var request pb.StatsStreamRequest
	if err := stream.RecvMsg(&request); err != nil {
		return err
	}
	return service.(agentServiceAPI).streamStats(&request, stream)
}

func metricsMatrixRPCHandler(service any, stream grpc.ServerStream) error {
	var request pb.MetricsMatrixRequest
	if err := stream.RecvMsg(&request); err != nil {
		return err
	}
	return service.(agentServiceAPI).streamMetricsMatrix(&request, stream)
}

func auditRPCHandler(service any, stream grpc.ServerStream) error {
	return service.(agentServiceAPI).syncAudit(stream)
}

var agentControlServiceDesc = grpc.ServiceDesc{
	ServiceName: "dockpilot.product.v1.AgentControl",
	HandlerType: (*agentServiceAPI)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Heartbeat", Handler: heartbeatRPCHandler},
		{MethodName: "Query", Handler: queryRPCHandler},
		{MethodName: "StartOperation", Handler: operationRPCHandler},
		{MethodName: "GetOperation", Handler: getOperationRPCHandler},
		{MethodName: "CancelOperation", Handler: cancelOperationRPCHandler},
		{MethodName: "ListActiveOperations", Handler: listActiveOperationsRPCHandler},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "SyncAudit", ServerStreams: true, ClientStreams: true, Handler: auditRPCHandler},
		{StreamName: "StreamLogs", ServerStreams: true, Handler: logsRPCHandler},
		{StreamName: "StreamStats", ServerStreams: true, Handler: statsRPCHandler},
		{StreamName: "StreamMetricsMatrix", ServerStreams: true, Handler: metricsMatrixRPCHandler},
	},
}

type singleConnDialer struct {
	mu   sync.Mutex
	conn net.Conn
}

func (d *singleConnDialer) DialContext(context.Context, string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, net.ErrClosed
	}
	conn := d.conn
	d.conn = nil
	return conn, nil
}

type singleConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	done      chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var accepted net.Conn
	l.once.Do(func() { accepted = l.conn })
	if accepted != nil {
		return accepted, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.conn.Close()
	})
	return err
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
