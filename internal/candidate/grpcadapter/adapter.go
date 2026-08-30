// Package grpcadapter implements Candidate A: an Agent-initiated TLS
// connection on which the Agent serves gRPC and the Server becomes the gRPC
// client through a dialer that returns only that accepted connection.
package grpcadapter

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	channelzservice "google.golang.org/grpc/channelz/service"
	"google.golang.org/grpc/credentials/insecure"
	grpcstats "google.golang.org/grpc/stats"

	"github.com/east-true/docklattice/internal/transport"
)

const alpn = "docklattice-grpc-prototype/1"
const initialFlowControlWindow = 64 << 10

type ConnectorConfig struct {
	Address   string
	TLSConfig *tls.Config
	AgentID   transport.AgentID
	Limits    transport.Limits
}

type Connector struct{ cfg ConnectorConfig }

func NewConnector(cfg ConnectorConfig) *Connector {
	if cfg.Limits.MaxMessageBytes == 0 {
		cfg.Limits = transport.DefaultLimits()
	}
	return &Connector{cfg: cfg}
}

func (c *Connector) Connect(ctx context.Context, h transport.Handler) (transport.Session, error) {
	if c.cfg.TLSConfig == nil {
		return nil, transport.Errorf(transport.CodeProtocol, "TLS config is required")
	}
	tlsConfig := c.cfg.TLSConfig.Clone()
	tlsConfig.NextProtos = []string{alpn}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.cfg.Address)
	if err != nil {
		return nil, transport.Wrap(transport.CodeUnavailable, err, "dial server")
	}
	conn := tls.Client(raw, tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, transport.Wrap(transport.CodeUnavailable, err, "TLS handshake")
	}
	info, err := agentHandshake(ctx, conn, c.cfg.AgentID, c.cfg.Limits)
	if err != nil {
		conn.Close()
		return nil, err
	}
	one := newSingleConnListener(conn)
	connectionEnded := make(chan struct{}, 1)
	dispatcher := newSendDispatcher()
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(c.cfg.Limits.MaxMessageBytes+1024),
		grpc.MaxSendMsgSize(c.cfg.Limits.MaxMessageBytes+1024),
		grpc.InitialWindowSize(initialFlowControlWindow),
		grpc.InitialConnWindowSize(initialFlowControlWindow),
		grpc.StatsHandler(connStats{ended: connectionEnded}),
	)
	registerService(server, &agentService{handler: h, limits: c.cfg.Limits, dispatcher: dispatcher})
	channelzservice.RegisterChannelzServiceToServer(server)
	s := newAgentSession(info, conn, server, one, dispatcher)
	go func() {
		err := server.Serve(one)
		dispatcher.stop()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			s.finish(transport.Wrap(transport.CodeUnavailable, err, "gRPC serve"))
		} else {
			s.finish(nil)
		}
	}()
	go func() {
		select {
		case <-connectionEnded:
			_ = one.Close()
			dispatcher.stop()
			s.finish(transport.Errorf(transport.CodeUnavailable, "reverse gRPC connection ended"))
		case <-s.Done():
		}
	}()
	return s, nil
}

type connStats struct{ ended chan<- struct{} }

func (connStats) TagRPC(ctx context.Context, _ *grpcstats.RPCTagInfo) context.Context   { return ctx }
func (connStats) HandleRPC(context.Context, grpcstats.RPCStats)                         {}
func (connStats) TagConn(ctx context.Context, _ *grpcstats.ConnTagInfo) context.Context { return ctx }
func (s connStats) HandleConn(_ context.Context, stat grpcstats.ConnStats) {
	if _, ok := stat.(*grpcstats.ConnEnd); ok {
		select {
		case s.ended <- struct{}{}:
		default:
		}
	}
}

type AcceptorConfig struct {
	Listener  net.Listener
	TLSConfig *tls.Config
	Limits    transport.Limits
}

type Acceptor struct {
	cfg    AcceptorConfig
	mu     sync.Mutex
	closed bool
}

func NewAcceptor(cfg AcceptorConfig) *Acceptor {
	if cfg.Limits.MaxMessageBytes == 0 {
		cfg.Limits = transport.DefaultLimits()
	}
	return &Acceptor{cfg: cfg}
}

func (a *Acceptor) Accept(ctx context.Context) (transport.Caller, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if a.cfg.Listener == nil || a.cfg.TLSConfig == nil {
		return nil, transport.Errorf(transport.CodeInternal, "listener and TLS config are required")
	}
	raw, err := acceptContext(ctx, a.cfg.Listener)
	if err != nil {
		return nil, transport.Wrap(transport.CodeUnavailable, err, "accept agent")
	}
	tlsConfig := a.cfg.TLSConfig.Clone()
	tlsConfig.NextProtos = []string{alpn}
	conn := tls.Server(raw, tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, transport.Wrap(transport.CodeUnavailable, err, "TLS handshake")
	}
	info, err := serverHandshake(ctx, conn, a.cfg.Limits)
	if err != nil {
		conn.Close()
		return nil, err
	}
	one := &singleConnDialer{conn: conn}
	cc, err := grpc.NewClient("passthrough:///reverse-agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(one.DialContext),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(a.cfg.Limits.MaxMessageBytes+1024),
			grpc.MaxCallSendMsgSize(a.cfg.Limits.MaxMessageBytes+1024),
		),
		grpc.WithInitialWindowSize(initialFlowControlWindow),
		grpc.WithInitialConnWindowSize(initialFlowControlWindow),
	)
	if err != nil {
		conn.Close()
		return nil, transport.Wrap(transport.CodeUnavailable, err, "create reverse gRPC client")
	}
	s := newCallerSession(info, cc, conn, a.cfg.Limits)
	cc.Connect()
	return s, nil
}

func (a *Acceptor) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	return a.cfg.Listener.Close()
}

func acceptContext(ctx context.Context, l net.Listener) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := l.Accept()
		ch <- result{conn, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

func agentHandshake(ctx context.Context, conn net.Conn, agentID transport.AgentID, limits transport.Limits) (transport.SessionInfo, error) {
	if len(agentID) == 0 || len(agentID) > 65535 {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid agent id length")
	}
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	header := make([]byte, 10+len(agentID))
	copy(header, "DPGA")
	binary.BigEndian.PutUint32(header[4:8], transport.ProtocolVersion)
	binary.BigEndian.PutUint16(header[8:10], uint16(len(agentID)))
	copy(header[10:], agentID)
	if _, err := conn.Write(header); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "write handshake")
	}
	response := make([]byte, 24)
	if _, err := io.ReadFull(conn, response); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "read handshake")
	}
	if string(response[:4]) != "DPGO" {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid handshake response")
	}
	version := binary.BigEndian.Uint32(response[4:8])
	if version < transport.MinCompatibleProtocolVersion || version > transport.ProtocolVersion {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "unsupported negotiated version %d", version)
	}
	return transport.SessionInfo{SessionID: transport.SessionID(fmt.Sprintf("%x", response[8:])), AgentID: agentID, ProtocolVersion: version, Limits: limits}, nil
}

func serverHandshake(ctx context.Context, conn net.Conn, limits transport.Limits) (transport.SessionInfo, error) {
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	header := make([]byte, 10)
	if _, err := io.ReadFull(conn, header); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "read handshake")
	}
	if string(header[:4]) != "DPGA" {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid handshake preface")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if version < transport.MinCompatibleProtocolVersion || version > transport.ProtocolVersion {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "unsupported agent protocol version %d", version)
	}
	length := binary.BigEndian.Uint16(header[8:10])
	if length == 0 {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "empty agent id")
	}
	id := make([]byte, int(length))
	if _, err := io.ReadFull(conn, id); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "read agent id")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeInternal, err, "generate session id")
	}
	response := make([]byte, 24)
	copy(response, "DPGO")
	binary.BigEndian.PutUint32(response[4:8], version)
	copy(response[8:], random)
	if _, err := conn.Write(response); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "write handshake")
	}
	return transport.SessionInfo{SessionID: transport.SessionID(fmt.Sprintf("%x", random)), AgentID: transport.AgentID(id), ProtocolVersion: version, Limits: limits}, nil
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
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
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
