package producttransport

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// preFeatureAgent is an Agent built before the metrics matrix existed: it
// serves stats, it does not implement the matrix handler at all, and it reports
// no matrix capability. It reports the protocol version it was given, which is
// the point of the tests below.
type preFeatureAgent struct{ statsCalls atomic.Int32 }

func (a *preFeatureAgent) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	// An Agent that predates the field does not set it. Silence is the answer,
	// and it must not be readable as anything else.
	return Capability{ConnectionReady: true, DockerReady: true}, nil
}

func (a *preFeatureAgent) StreamStats(_ context.Context, _ SessionInfo, request StatsRequest, sender StatsSender) error {
	a.statsCalls.Add(1)
	return sender.Send(StatsSample{ContainerID: request.ContainerID, CPUPercent: 12.5})
}

// featureAgent is the same Agent after the feature exists. Nothing about its
// protocol version differs from preFeatureAgent.
type featureAgent struct {
	matrixCalls atomic.Int32
	statsCalls  atomic.Int32
}

func (a *featureAgent) Heartbeat(context.Context, SessionInfo, time.Time) (Capability, error) {
	return Capability{ConnectionReady: true, DockerReady: true, MetricsMatrix: true}, nil
}

func (a *featureAgent) StreamStats(_ context.Context, _ SessionInfo, request StatsRequest, sender StatsSender) error {
	a.statsCalls.Add(1)
	return sender.Send(StatsSample{ContainerID: request.ContainerID, CPUPercent: 12.5})
}

func (a *featureAgent) StreamMetricsMatrix(_ context.Context, _ SessionInfo, _ MetricsMatrixRequest, sender MetricsMatrixSender) error {
	a.matrixCalls.Add(1)
	return sender.Send(MetricsMatrixFrame{
		ObservedAt: time.Unix(1700000000, 0).UTC(),
		Workload:   WorkloadSummary{CPUCapacity: 4, MemoryCapacity: 8 << 30, ContainersRunning: 1, ContainersTotal: 1},
		Containers: []StatsSample{{ContainerID: "container-a", CPUPercent: 5}},
	})
}

// connectedPair brings up a real TLS reverse-gRPC session at the requested
// protocol version and hands back the Server's side of it.
func connectedPair(t *testing.T, version uint32, handler AgentHandler) ControlSession {
	t.Helper()
	serverTLS, agentTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewServerAcceptor(ServerConfig{
		Listener: listener, TLSConfig: serverTLS, Registry: durableTestRegistry(), HeartbeatInterval: time.Hour,
		Verifier: CredentialVerifierFunc(func(context.Context, []byte, time.Time) (CredentialIdentity, error) {
			return CredentialIdentity{AgentID: "agent-compat", CredentialID: "credential", ServerIdentityID: "server"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acceptor.Close() })

	accepted := make(chan ControlSession, 1)
	failed := make(chan error, 1)
	go func() {
		session, acceptErr := acceptor.Accept(context.Background())
		if acceptErr != nil {
			failed <- acceptErr
			return
		}
		accepted <- session
	}()
	connector, err := NewAgentConnector(AgentConfig{
		Address: listener.Addr().String(), TLSConfig: agentTLS, Credential: []byte("compat-credential"),
		Incarnation: 3, ProtocolVersion: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := connector.Connect(context.Background(), handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentSession.Close(nil) })

	select {
	case err := <-failed:
		t.Fatalf("accept: %v", err)
	case session := <-accepted:
		t.Cleanup(func() { _ = session.Close(nil) })
		if got := session.Info().ProtocolVersion; got != version {
			t.Fatalf("negotiated version = %d, want %d", got, version)
		}
		return session
	case <-time.After(5 * time.Second):
		t.Fatal("no session was accepted")
	}
	return nil
}

// The central claim of the capability design, proved over a real connection:
// an Agent built before this feature and one built after report the *same*
// protocol version, so the version cannot tell them apart and the capability
// flag is the only thing that can. This holds at N and at N-1 alike.
func TestMetricsCapabilityRatherThanVersionDistinguishesAgents(t *testing.T) {
	for _, version := range []uint32{CurrentProductProtocolVersion, PreviousProductProtocolVersion} {
		t.Run(protocolVersionName(version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			old := &preFeatureAgent{}
			oldSession := connectedPair(t, version, old)
			oldBeat, err := oldSession.Heartbeat(ctx)
			if err != nil {
				t.Fatalf("heartbeat from the pre-feature Agent: %v", err)
			}
			if oldBeat.Capability.MetricsMatrix {
				t.Fatal("an Agent that never set the field was read as reporting it")
			}

			current := &featureAgent{}
			newSession := connectedPair(t, version, current)
			newBeat, err := newSession.Heartbeat(ctx)
			if err != nil {
				t.Fatalf("heartbeat from the current Agent: %v", err)
			}
			if !newBeat.Capability.MetricsMatrix {
				t.Fatal("an Agent that serves the matrix did not report it")
			}

			if oldSession.Info().ProtocolVersion != newSession.Info().ProtocolVersion {
				t.Fatal("the two Agents differ by protocol version, which would make this whole test moot")
			}
		})
	}
}

// Capability negotiation decides what a Server should call. It is not what
// keeps an Agent alive when something calls anyway. A matrix request reaching
// an Agent that cannot serve it is answered, the session stays usable, and the
// Agent's other streams are unaffected.
func TestMatrixRequestToAPreFeatureAgentFailsClosed(t *testing.T) {
	for _, version := range []uint32{CurrentProductProtocolVersion, PreviousProductProtocolVersion} {
		t.Run(protocolVersionName(version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			agent := &preFeatureAgent{}
			session := connectedPair(t, version, agent)

			stream, err := session.OpenMetricsMatrix(ctx, MetricsMatrixRequest{})
			if err != nil {
				// Refusing at open is an acceptable answer; what must not
				// happen is a panic, a hang, or an untyped failure.
				if !errors.Is(err, ErrHandlerUnavailable) {
					t.Fatalf("opening a matrix stream answered %v, want the unavailable error", err)
				}
			} else {
				defer stream.Close()
				if _, err := stream.Recv(ctx); !errors.Is(err, ErrHandlerUnavailable) {
					t.Fatalf("receiving from an unserved matrix stream answered %v, want the unavailable error", err)
				}
			}

			// The Agent is still there, and the stream it has always served
			// still works. A refused new capability must not cost an operator
			// the ones they already had.
			if _, err := session.Heartbeat(ctx); err != nil {
				t.Fatalf("the session did not survive the refused matrix request: %v", err)
			}
			stats, err := session.OpenStats(ctx, StatsRequest{ContainerID: "container-a"})
			if err != nil {
				t.Fatalf("open legacy stats after a refused matrix request: %v", err)
			}
			defer stats.Close()
			sample, err := stats.Recv(ctx)
			if err != nil {
				t.Fatalf("legacy stats stream: %v", err)
			}
			if sample.ContainerID != "container-a" || sample.CPUPercent != 12.5 {
				t.Fatalf("legacy stats sample = %+v", sample)
			}
			if got := agent.statsCalls.Load(); got != 1 {
				t.Fatalf("legacy stats handler ran %d times", got)
			}
		})
	}
}

// The Agent that does serve it, serves it, and a whole frame survives the wire
// intact at both supported protocol versions.
func TestMatrixFrameSurvivesTheWireAtBothVersions(t *testing.T) {
	for _, version := range []uint32{CurrentProductProtocolVersion, PreviousProductProtocolVersion} {
		t.Run(protocolVersionName(version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			agent := &featureAgent{}
			session := connectedPair(t, version, agent)

			stream, err := session.OpenMetricsMatrix(ctx, MetricsMatrixRequest{})
			if err != nil {
				t.Fatalf("open matrix: %v", err)
			}
			defer stream.Close()
			frame, err := stream.Recv(ctx)
			if err != nil {
				t.Fatalf("receive frame: %v", err)
			}
			if frame.Workload.CPUCapacity != 4 || frame.Workload.MemoryCapacity != 8<<30 {
				t.Fatalf("host row did not survive the wire: %+v", frame.Workload)
			}
			if len(frame.Containers) != 1 || frame.Containers[0].ContainerID != "container-a" {
				t.Fatalf("container rows did not survive the wire: %+v", frame.Containers)
			}
			if got := agent.matrixCalls.Load(); got != 1 {
				t.Fatalf("the Agent's matrix handler ran %d times, want once", got)
			}
		})
	}
}
