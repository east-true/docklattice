// Package wsadapter implements Candidate B: WSS plus application framing,
// credit-based per-stream flow control, and class-aware bounded scheduling.
package wsadapter

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/east-true/dockpilot/internal/transport"
)

const Path = "/prototype/agent"

type ConnectorConfig struct {
	URL       string
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
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: c.cfg.TLSConfig.Clone()}}
	ws, _, err := websocket.Dial(ctx, c.cfg.URL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, transport.Wrap(transport.CodeUnavailable, err, "dial WSS server")
	}
	conn := &abruptConn{Conn: websocket.NetConn(context.Background(), ws, websocket.MessageBinary), ws: ws}
	info, err := agentHandshake(ctx, conn, c.cfg.AgentID, c.cfg.Limits)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return newMux(conn, info, h, false), nil
}

type AcceptorConfig struct {
	Listener  net.Listener
	TLSConfig *tls.Config
	Limits    transport.Limits
}

type Acceptor struct {
	cfg      AcceptorConfig
	once     sync.Once
	server   *http.Server
	accepted chan acceptResult
	mu       sync.Mutex
	closed   bool
}

type acceptResult struct {
	caller transport.Caller
	err    error
}

func NewAcceptor(cfg AcceptorConfig) *Acceptor {
	if cfg.Limits.MaxMessageBytes == 0 {
		cfg.Limits = transport.DefaultLimits()
	}
	return &Acceptor{cfg: cfg, accepted: make(chan acceptResult, 16)}
}

func (a *Acceptor) start() {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, a.handle)
	a.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	tlsListener := tls.NewListener(a.cfg.Listener, a.cfg.TLSConfig)
	go func() {
		if err := a.server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			select {
			case a.accepted <- acceptResult{err: transport.Wrap(transport.CodeUnavailable, err, "serve WSS")}:
			default:
			}
		}
	}()
}

func (a *Acceptor) handle(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	conn := &abruptConn{Conn: websocket.NetConn(context.Background(), ws, websocket.MessageBinary), ws: ws}
	info, err := serverHandshake(r.Context(), conn, a.cfg.Limits)
	if err != nil {
		_ = conn.Close()
		select {
		case a.accepted <- acceptResult{err: err}:
		default:
		}
		return
	}
	caller := newMux(conn, info, nil, true)
	select {
	case a.accepted <- acceptResult{caller: caller}:
	case <-r.Context().Done():
		_ = caller.Close(r.Context().Err())
		return
	}
	<-caller.Done()
}

func (a *Acceptor) Accept(ctx context.Context) (transport.Caller, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	a.once.Do(a.start)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-a.accepted:
		return result.caller, result.err
	}
}

func (a *Acceptor) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	if a.server != nil {
		_ = a.server.Close()
	}
	return a.cfg.Listener.Close()
}

func agentHandshake(ctx context.Context, conn net.Conn, agentID transport.AgentID, limits transport.Limits) (transport.SessionInfo, error) {
	if len(agentID) == 0 || len(agentID) > 65535 {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid agent id length")
	}
	setDeadline(ctx, conn)
	defer conn.SetDeadline(time.Time{})
	header := make([]byte, 10+len(agentID))
	copy(header, "DPWA")
	binary.BigEndian.PutUint32(header[4:8], transport.ProtocolVersion)
	binary.BigEndian.PutUint16(header[8:10], uint16(len(agentID)))
	copy(header[10:], agentID)
	if err := writeFull(conn, header); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "write handshake")
	}
	response := make([]byte, 24)
	if _, err := io.ReadFull(conn, response); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "read handshake")
	}
	if string(response[:4]) != "DPWO" {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid handshake response")
	}
	version := binary.BigEndian.Uint32(response[4:8])
	if version < transport.MinCompatibleProtocolVersion || version > transport.ProtocolVersion {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "unsupported negotiated protocol version %d", version)
	}
	return transport.SessionInfo{SessionID: transport.SessionID(fmt.Sprintf("%x", response[8:])), AgentID: agentID, ProtocolVersion: version, Limits: limits}, nil
}

func serverHandshake(ctx context.Context, conn net.Conn, limits transport.Limits) (transport.SessionInfo, error) {
	setDeadline(ctx, conn)
	defer conn.SetDeadline(time.Time{})
	header := make([]byte, 10)
	if _, err := io.ReadFull(conn, header); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "read handshake")
	}
	if string(header[:4]) != "DPWA" {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "invalid handshake preface")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if version < transport.MinCompatibleProtocolVersion || version > transport.ProtocolVersion {
		return transport.SessionInfo{}, transport.Errorf(transport.CodeProtocol, "unsupported protocol version %d", version)
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
	copy(response, "DPWO")
	binary.BigEndian.PutUint32(response[4:8], version)
	copy(response[8:], random)
	if err := writeFull(conn, response); err != nil {
		return transport.SessionInfo{}, transport.Wrap(transport.CodeUnavailable, err, "write handshake response")
	}
	return transport.SessionInfo{SessionID: transport.SessionID(fmt.Sprintf("%x", random)), AgentID: transport.AgentID(id), ProtocolVersion: version, Limits: limits}, nil
}

func setDeadline(ctx context.Context, conn net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
}

type abruptConn struct {
	net.Conn
	ws *websocket.Conn
}

func (c *abruptConn) Close() error { return c.ws.CloseNow() }
