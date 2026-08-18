package wsadapter

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/transport"
	"github.com/east-true/dockpilot/internal/transport/conformance"
)

func TestConformance(t *testing.T) { conformance.Run(t, testFactory(t)) }

func testFactory(t *testing.T) conformance.Factory {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	return func(ctx context.Context, h transport.Handler, limits transport.Limits) (transport.Caller, func(), error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		acceptor := NewAcceptor(AcceptorConfig{Listener: listener, TLSConfig: serverTLS, Limits: limits})
		type result struct {
			caller transport.Caller
			err    error
		}
		accepted := make(chan result, 1)
		go func() {
			caller, err := acceptor.Accept(ctx)
			accepted <- result{caller, err}
		}()
		connector := NewConnector(ConnectorConfig{URL: "wss://" + listener.Addr().String() + Path, TLSConfig: clientTLS, AgentID: "agent-test", Limits: limits})
		agent, err := connector.Connect(ctx, h)
		if err != nil {
			_ = acceptor.Close()
			return nil, nil, err
		}
		got := <-accepted
		if got.err != nil {
			_ = agent.Close(got.err)
			_ = acceptor.Close()
			return nil, nil, got.err
		}
		cleanup := func() {
			_ = got.caller.Close(nil)
			_ = agent.Close(nil)
			_ = acceptor.Close()
		}
		return got.caller, cleanup, nil
	}
}

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dockpilot-prototype"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}
}
