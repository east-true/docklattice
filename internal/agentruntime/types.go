// Package agentruntime assembles the executable Agent boot and outbound
// connection lifecycle from the durable state, WAL, registration, Docker, and
// product transport packages.
package agentruntime

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/east-true/dockpilot/internal/agentstate"
	"github.com/east-true/dockpilot/internal/agentstorage"
	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/registrationhttp"
)

var (
	ErrInvalidConfig      = errors.New("agentruntime: invalid config")
	ErrCredentialRequired = errors.New("agentruntime: credential or Join Token required")
	ErrCredentialIdentity = errors.New("agentruntime: credential identity mismatch")
	ErrClosed             = errors.New("agentruntime: closed")
	ErrAlreadyRunning     = errors.New("agentruntime: connection maintenance already running")
)

type Clock interface{ Now() time.Time }

type Docker interface {
	Probe(context.Context) (dockeradapter.Capability, error)
	List(context.Context) ([]dockeradapter.Container, error)
	Close() error
}

type DockerOpenFunc func(dockeradapter.IdentityProvider) (Docker, error)

// ConnectFunc receives the currently durable credential on every reconnect.
// The default implementation creates a TLS producttransport.AgentConnector.
type ConnectFunc func(context.Context, []byte, uint64, producttransport.AgentHandler) (producttransport.Session, error)

type Config struct {
	StateDir string
	WALDir   string

	Registration *registrationhttp.Client
	JoinToken    string
	DisplayName  string
	Metadata     map[string]string

	ServerAddress string
	TLSConfig     *tls.Config
	// PeerSilenceTimeout is the window after which a Server that has stopped
	// calling is treated as gone and the session is closed so the reconnect
	// loop runs. It mirrors the Server's own offline threshold; zero selects
	// producttransport.DefaultPeerSilenceTimeout.
	PeerSilenceTimeout time.Duration
	Connect            ConnectFunc

	WALOptions      auditwal.Options
	ReconnectPolicy producttransport.ReconnectPolicy
	Sleeper         producttransport.Sleeper
	Random          producttransport.Random

	DockerOpen            DockerOpenFunc
	Self                  agentsafety.SelfConfig
	BundledComposeVersion string
	ProjectRoots          []string
	DiscoveryInterval     time.Duration
	Now                   func() time.Time

	// Test seams for deterministic pressure/fault matrices. Production leaves
	// both zero-valued and uses the real observer and v1 defaults.
	storageObserve   agentstorage.Observer
	storageBudget    diskbudget.Config
	projectEvaluator agentprojects.Evaluator
}

type Runtime struct {
	mu              sync.Mutex
	config          Config
	state           *agentstate.Store
	wal             *auditwal.WAL
	docker          Docker
	startup         agentstate.Startup
	heartbeat       *heartbeatHandler
	handler         producttransport.AgentHandler
	credential      identity.Credential
	credentialState agentstate.Credential
	connect         ConnectFunc
	closeRequested  bool
	closeInProgress bool
	closed          bool
	closeAttempt    *runtimeCloseAttempt
	closeErr        error
	maintainCancel  context.CancelFunc
	maintainDone    chan struct{}
	eventCancel     context.CancelFunc
	eventDone       chan error
	identification  *identificationState
	rootAssessments map[string]agentsafety.RootAssessment
	productCloser   interface{ Close() error }
	operationEngine interface{ Shutdown(context.Context) error }
	productStorage  *runtimeStorage
	discoveryCancel context.CancelFunc
	discoveryDone   chan struct{}
}

type runtimeCloseAttempt struct {
	done chan struct{}
	err  error
}

type dockerEventSource interface {
	auditevents.Source
}

type dockerInspector interface {
	auditevents.Inspector
}

func (r *Runtime) Startup() agentstate.Startup { return r.startup }
func (r *Runtime) WAL() *auditwal.WAL          { return r.wal }

func (r *Runtime) Snapshot() (agentstate.Snapshot, error) {
	if r.state == nil {
		return agentstate.Snapshot{}, ErrClosed
	}
	return r.state.Snapshot()
}

type identificationState struct {
	mu    sync.RWMutex
	value agentsafety.Identification
}

func (s *identificationState) get() agentsafety.Identification {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneIdentification(s.value)
}
func (s *identificationState) set(value agentsafety.Identification) {
	s.mu.Lock()
	s.value = cloneIdentification(value)
	s.mu.Unlock()
}

func cloneIdentification(value agentsafety.Identification) agentsafety.Identification {
	value.SelectedAgentIDs = append([]string(nil), value.SelectedAgentIDs...)
	value.ProtectedContainerIDs = append([]string(nil), value.ProtectedContainerIDs...)
	value.ProtectedComposeProjects = append([]string(nil), value.ProtectedComposeProjects...)
	return value
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
