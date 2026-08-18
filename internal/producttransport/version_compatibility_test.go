package producttransport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestReverseGRPCProductProtocolNAndNMinusOneCompatibility(t *testing.T) {
	for _, version := range []uint32{CurrentProductProtocolVersion, PreviousProductProtocolVersion} {
		version := version
		t.Run(protocolVersionName(version), func(t *testing.T) {
			serverTLS, agentTLS := testTLS(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var verified atomic.Int32
			acceptor, err := NewServerAcceptor(ServerConfig{
				Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(), HeartbeatInterval: time.Hour,
				Verifier: CredentialVerifierFunc(func(_ context.Context, credential []byte, _ time.Time) (CredentialIdentity, error) {
					verified.Add(1)
					if string(credential) != "versioned-credential" {
						return CredentialIdentity{}, errors.New("bad credential")
					}
					return CredentialIdentity{AgentID: "agent-version", CredentialID: "credential", ServerIdentityID: "server"}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer acceptor.Close()

			accepted := make(chan struct {
				session ControlSession
				err     error
			}, 1)
			go func() {
				session, acceptErr := acceptor.Accept(context.Background())
				accepted <- struct {
					session ControlSession
					err     error
				}{session: session, err: acceptErr}
			}()
			connector, err := NewAgentConnector(AgentConfig{
				Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("versioned-credential"),
				Incarnation: 7, ProtocolVersion: version,
			})
			if err != nil {
				t.Fatal(err)
			}
			handled := make(chan SessionInfo, 1)
			agentSession, err := connector.Connect(context.Background(), AgentHandlerFunc(func(_ context.Context, info SessionInfo, _ time.Time) (Capability, error) {
				handled <- info
				return Capability{ConnectionReady: true}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer agentSession.Close(nil)
			result := <-accepted
			if result.err != nil {
				t.Fatal(result.err)
			}
			defer result.session.Close(nil)
			if got := agentSession.Info().ProtocolVersion; got != version {
				t.Fatalf("Agent negotiated version = %d, want %d", got, version)
			}
			if got := result.session.Info().ProtocolVersion; got != version {
				t.Fatalf("Server negotiated version = %d, want %d", got, version)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := result.session.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			recovery, ok := result.session.(OperationRecoverySession)
			if !ok {
				t.Fatal("control session does not expose optional recovery capability")
			}
			if _, err := recovery.ListActiveOperations(ctx, ListActiveOperationsRequest{}); !errors.Is(err, ErrHandlerUnavailable) {
				t.Fatalf("version %d missing recovery capability error = %v", version, err)
			}
			select {
			case info := <-handled:
				if info.ProtocolVersion != version {
					t.Fatalf("gRPC handler version = %d, want %d", info.ProtocolVersion, version)
				}
			case <-ctx.Done():
				t.Fatal("negotiated reverse gRPC heartbeat did not reach Agent")
			}
			if got := verified.Load(); got != 1 {
				t.Fatalf("credential verification calls = %d, want 1", got)
			}
		})
	}
}

func TestProductProtocolRejectsTooOldAndFutureAtServerHandshake(t *testing.T) {
	versions := []struct {
		name    string
		version uint32
	}{
		{name: "too-old", version: PreviousProductProtocolVersion - 1},
		{name: "future", version: CurrentProductProtocolVersion + 1},
	}
	for _, test := range versions {
		t.Run(test.name, func(t *testing.T) {
			serverTLS, agentTLS := testTLS(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var verified atomic.Int32
			acceptor, err := NewServerAcceptor(ServerConfig{
				Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(), HeartbeatInterval: time.Hour,
				Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
					verified.Add(1)
					return CredentialIdentity{AgentID: "should-not-authenticate", CredentialID: "credential", ServerIdentityID: "server"}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer acceptor.Close()
			accepted := make(chan error, 1)
			go func() {
				_, acceptErr := acceptor.Accept(context.Background())
				accepted <- acceptErr
			}()

			raw, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			connection := tls.Client(raw, productTLSConfig(agentTLS))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := connection.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			_, agentErr := agentHandshakeVersion(ctx, connection, []byte("unverified"), 1, DefaultMaxCredentialBytes, test.version)
			_ = connection.Close()
			if agentErr == nil {
				t.Fatalf("Agent version %d unexpectedly completed handshake", test.version)
			}
			select {
			case serverErr := <-accepted:
				if !errors.Is(serverErr, ErrProtocol) {
					t.Fatalf("Server version %d error = %v", test.version, serverErr)
				}
			case <-ctx.Done():
				t.Fatal("Server did not reject incompatible protocol version")
			}
			if got := verified.Load(); got != 0 {
				t.Fatalf("incompatible version reached credential verifier %d times", got)
			}
		})
	}
}

func TestNMinusOneStillRequiresCredentialAuthentication(t *testing.T) {
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("credential denied")
	var verified atomic.Int32
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(), HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			verified.Add(1)
			return CredentialIdentity{}, denied
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer acceptor.Close()
	accepted := make(chan error, 1)
	go func() {
		_, acceptErr := acceptor.Accept(context.Background())
		accepted <- acceptErr
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("invalid"),
		Incarnation: 1, ProtocolVersion: PreviousProductProtocolVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session, connectErr := connector.Connect(context.Background(), AgentHandlerFunc(func(context.Context, SessionInfo, time.Time) (Capability, error) {
		return Capability{}, nil
	})); connectErr == nil {
		_ = session.Close(nil)
		t.Fatal("N-1 Agent bypassed credential authentication")
	}
	serverErr := <-accepted
	if !errors.Is(serverErr, ErrAuthentication) || !errors.Is(serverErr, denied) {
		t.Fatalf("N-1 authentication error = %v", serverErr)
	}
	if got := verified.Load(); got != 1 {
		t.Fatalf("N-1 credential verification calls = %d, want 1", got)
	}
}

func protocolVersionName(version uint32) string {
	if version == CurrentProductProtocolVersion {
		return "N"
	}
	return "N-1"
}
