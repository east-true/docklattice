package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/east-true/dockpilot/internal/agentstate"
	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditsync"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/registrationhttp"
)

func Boot(ctx context.Context, config Config) (_ *Runtime, err error) {
	if err := normalizeConfig(&config); err != nil {
		return nil, err
	}
	inspection, err := agentstate.Inspect(config.StateDir)
	if err != nil {
		return nil, fmt.Errorf("agentruntime: inspect identity: %w", err)
	}
	recovery, err := auditwal.Recover(config.WALDir, inspection.AgentID, config.WALOptions)
	if err != nil {
		return nil, fmt.Errorf("agentruntime: recover audit WAL: %w", err)
	}
	var tail *agentstate.Cursor
	if recovery.WALTail != nil {
		tail = &agentstate.Cursor{Incarnation: recovery.WALTail.Incarnation, Seq: recovery.WALTail.Seq}
	}
	state, startup, err := agentstate.Open(ctx, config.StateDir, tail)
	if err != nil {
		return nil, fmt.Errorf("agentruntime: open Agent state: %w", err)
	}
	defer func() {
		if err != nil {
			_ = state.Close()
		}
	}()
	if inspection.Exists && startup.AgentID != inspection.AgentID {
		return nil, fmt.Errorf("%w: Agent identity changed between WAL recovery and state open", ErrCredentialIdentity)
	}

	wal, err := auditwal.Open(config.WALDir, startup.AgentID, startup.CurrentIncarnation, config.WALOptions)
	if err != nil {
		return nil, fmt.Errorf("agentruntime: open audit WAL: %w", err)
	}
	defer func() {
		if err != nil {
			_ = wal.Close()
		}
	}()

	runtime := &Runtime{config: config, state: state, wal: wal, startup: startup}
	defer func() {
		if err != nil {
			runtime.stopDiscovery()
			_ = closeProduct(runtime.productCloser)
			_ = runtime.stopObservedAudit()
			_ = closeDocker(runtime.docker)
		}
	}()
	runtime.heartbeat = &heartbeatHandler{agentID: startup.AgentID, incarnation: startup.CurrentIncarnation}
	runtime.handler = runtime.heartbeat
	runtime.connect = config.Connect
	if runtime.connect == nil {
		runtime.connect = defaultConnect(config)
	}

	if err := runtime.loadOrRegisterCredential(ctx); err != nil {
		return nil, err
	}
	if err := runtime.recoverPendingActivation(ctx); err != nil {
		return nil, err
	}
	if err := runtime.renewIfDue(ctx); err != nil {
		return nil, err
	}
	if err := runtime.reconcileArchive(); err != nil {
		return nil, err
	}
	if err := runtime.appendContinuityBoundary(ctx); err != nil {
		return nil, err
	}
	if err := runtime.startAuditSync(); err != nil {
		return nil, err
	}
	if err := runtime.startDocker(ctx); err != nil {
		return nil, err
	}
	if err := runtime.startProduct(ctx); err != nil {
		return nil, err
	}
	if err := runtime.startObservedAudit(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}

// Run boots, maintains the outbound connection until cancellation, then
// performs the ordered graceful shutdown.
func Run(ctx context.Context, config Config) error {
	runtime, err := Boot(ctx, config)
	if err != nil {
		return err
	}
	runErr := runtime.Maintain(ctx)
	closeErr := runtime.Close(context.Background())
	return errors.Join(runErr, closeErr)
}

func (r *Runtime) Maintain(ctx context.Context) error {
	r.mu.Lock()
	if r.closeRequested {
		r.mu.Unlock()
		return ErrClosed
	}
	if r.maintainDone != nil {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	maintainCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.maintainCancel, r.maintainDone = cancel, done
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.maintainCancel = nil
		close(done)
		r.mu.Unlock()
	}()
	return producttransport.Maintain(maintainCtx, func(connectCtx context.Context) (producttransport.Session, error) {
		if err := r.renewIfDue(connectCtx); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.closeRequested {
			r.mu.Unlock()
			return nil, ErrClosed
		}
		credential := r.credential
		incarnation := r.startup.CurrentIncarnation
		r.mu.Unlock()
		payload, err := identity.MarshalCredential(credential)
		if err != nil {
			return nil, err
		}
		defer clear(payload)
		return r.connect(connectCtx, payload, incarnation, r.handler)
	}, r.config.ReconnectPolicy, r.config.Sleeper, r.config.Random, nil)
}

func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.closeInProgress {
		attempt := r.closeAttempt
		r.mu.Unlock()
		<-attempt.done
		return attempt.err
	}
	r.closeRequested = true
	r.closeInProgress = true
	attempt := &runtimeCloseAttempt{done: make(chan struct{})}
	r.closeAttempt = attempt
	maintainCancel, maintainDone := r.maintainCancel, r.maintainDone
	r.mu.Unlock()
	if maintainCancel != nil {
		maintainCancel()
		<-maintainDone
	}
	r.stopDiscovery()
	if r.operationEngine != nil {
		if shutdownErr := r.operationEngine.Shutdown(ctx); shutdownErr != nil {
			r.finishCloseAttempt(attempt, false, shutdownErr)
			return shutdownErr
		}
	}
	productErr := closeProduct(r.productCloser)
	eventErr := r.stopObservedAudit()
	var result error
	if err := r.wal.Sync(); err != nil {
		walCloseErr := r.wal.Close()
		dockerErr := closeDocker(r.docker)
		stateErr := r.state.Close()
		result = errors.Join(productErr, eventErr, err, walCloseErr, dockerErr, stateErr)
	} else {
		bounds, err := r.wal.Bounds()
		if err != nil {
			result = errors.Join(productErr, eventErr, err, r.wal.Close(), closeDocker(r.docker), r.state.Close())
		} else {
			lastDurableSeq := uint64(0)
			if bounds.DurableThrough != nil && bounds.DurableThrough.Incarnation == r.startup.CurrentIncarnation {
				lastDurableSeq = bounds.DurableThrough.Seq
			}
			walErr := r.wal.Close()
			dockerErr := closeDocker(r.docker)
			if walErr != nil {
				result = errors.Join(productErr, eventErr, walErr, dockerErr, r.state.Close())
			} else {
				stateErr := r.state.GracefulClose(ctx, lastDurableSeq, r.config.Now())
				// GracefulClose intentionally keeps the Store lock open when its
				// durable clean-close write fails. The process lifecycle cannot
				// retain that lock forever, so release it through the explicitly
				// unclean fallback and surface both errors.
				var stateFallbackErr error
				if stateErr != nil {
					stateFallbackErr = r.state.Close()
				}
				result = errors.Join(productErr, eventErr, dockerErr, stateErr, stateFallbackErr)
			}
		}
	}
	r.finishCloseAttempt(attempt, true, result)
	return result
}

// finishCloseAttempt keeps a failed Engine shutdown retryable. The operation
// Engine deliberately leaves its WAL, Docker, and journal dependencies open
// when its bound expires, because an in-flight runner may still be using them.
func (r *Runtime) finishCloseAttempt(attempt *runtimeCloseAttempt, complete bool, result error) {
	r.mu.Lock()
	attempt.err = result
	r.closeErr = result
	r.closed = complete
	r.closeInProgress = false
	close(attempt.done)
	r.mu.Unlock()
}

func closeDocker(docker Docker) error {
	if docker == nil {
		return nil
	}
	return docker.Close()
}

func closeProduct(product interface{ Close() error }) error {
	if product == nil {
		return nil
	}
	return product.Close()
}

func normalizeConfig(config *Config) error {
	if config.StateDir == "" || config.WALDir == "" {
		return fmt.Errorf("%w: state and WAL directories are required", ErrInvalidConfig)
	}
	stateDir, err := filepath.Abs(config.StateDir)
	if err != nil {
		return fmt.Errorf("%w: state directory: %v", ErrInvalidConfig, err)
	}
	walDir, err := filepath.Abs(config.WALDir)
	if err != nil {
		return fmt.Errorf("%w: WAL directory: %v", ErrInvalidConfig, err)
	}
	if stateDir == walDir {
		return fmt.Errorf("%w: state and WAL directories must differ", ErrInvalidConfig)
	}
	config.StateDir, config.WALDir = stateDir, walDir
	seenRoots := make(map[string]struct{}, len(config.ProjectRoots))
	normalizedRoots := make([]string, 0, len(config.ProjectRoots))
	for _, root := range config.ProjectRoots {
		absolute, err := filepath.Abs(root)
		if err != nil || absolute != filepath.Clean(root) {
			return fmt.Errorf("%w: project root %q must be absolute and clean", ErrInvalidConfig, root)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return fmt.Errorf("%w: resolve project root %q: %v", ErrInvalidConfig, root, err)
		}
		canonical = filepath.Clean(canonical)
		if _, duplicate := seenRoots[canonical]; duplicate {
			return fmt.Errorf("%w: duplicate project root %q", ErrInvalidConfig, root)
		}
		seenRoots[canonical] = struct{}{}
		normalizedRoots = append(normalizedRoots, canonical)
	}
	config.ProjectRoots = normalizedRoots
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.DiscoveryInterval == 0 {
		config.DiscoveryInterval = 5 * time.Minute
	}
	if config.DiscoveryInterval < 0 {
		return fmt.Errorf("%w: discovery interval must be positive", ErrInvalidConfig)
	}
	if config.ReconnectPolicy.Initial == 0 {
		config.ReconnectPolicy = producttransport.DefaultReconnectPolicy()
	}
	if config.Connect == nil && (config.ServerAddress == "" || config.TLSConfig == nil) {
		return fmt.Errorf("%w: Server address and TLS configuration are required", ErrInvalidConfig)
	}
	if config.DockerOpen == nil {
		config.DockerOpen = func(provider dockeradapter.IdentityProvider) (Docker, error) {
			return dockeradapter.OpenFromEnv(provider)
		}
	}
	return nil
}

func defaultConnect(config Config) ConnectFunc {
	return func(ctx context.Context, credential []byte, incarnation uint64, handler producttransport.AgentHandler) (producttransport.Session, error) {
		connector, err := producttransport.NewAgentConnector(producttransport.AgentConfig{
			Address: config.ServerAddress, TLSConfig: config.TLSConfig,
			Credential: credential, Incarnation: incarnation,
			PeerSilenceTimeout: config.PeerSilenceTimeout,
		})
		if err != nil {
			return nil, err
		}
		return connector.Connect(ctx, handler)
	}
}

func (r *Runtime) loadOrRegisterCredential(ctx context.Context) error {
	snapshot, err := r.state.Snapshot()
	if err != nil {
		return err
	}
	if credentialMaterialPresent(snapshot.Credential) {
		credential, err := loadCredential(snapshot.Credential)
		if err != nil {
			return fmt.Errorf("agentruntime: load credential: %w", err)
		}
		if err := validateCredentialIdentity(credential, snapshot); err != nil {
			return err
		}
		r.credential, r.credentialState = credential, snapshot.Credential
		r.heartbeat.setCredentialIdentity(credential.CredentialID, credential.ServerIdentityID)
		if !r.config.Now().Before(credential.ExpiresAt) {
			if r.config.JoinToken == "" {
				return fmt.Errorf("%w: existing credential expired", ErrCredentialRequired)
			}
			return r.register(ctx, &credential)
		}
		return nil
	}
	return r.register(ctx, nil)
}

func (r *Runtime) register(ctx context.Context, expired *identity.Credential) error {
	if r.config.Registration == nil || r.config.JoinToken == "" || r.config.DisplayName == "" {
		return ErrCredentialRequired
	}
	response, err := r.config.Registration.Register(ctx, registrationhttp.RegisterRequest{
		JoinToken: r.config.JoinToken, AgentID: r.startup.AgentID, DisplayName: r.config.DisplayName,
		Metadata: cloneMetadata(r.config.Metadata), ExpiredCredential: expired,
	})
	if err != nil {
		return fmt.Errorf("agentruntime: register: %w", err)
	}
	return r.installCredentialAndArchive(ctx, response)
}

func (r *Runtime) recoverPendingActivation(ctx context.Context) error {
	snapshot, err := r.state.Snapshot()
	if err != nil {
		return err
	}
	if snapshot.PendingActivation == nil {
		return nil
	}
	if r.config.Registration == nil {
		return fmt.Errorf("agentruntime: pending credential activation requires registration client")
	}
	previous, err := loadCredential(snapshot.PendingActivation.Previous)
	if err != nil {
		return fmt.Errorf("agentruntime: load pending previous credential: %w", err)
	}
	if r.credential.CredentialID != snapshot.PendingActivation.ActiveCredentialID {
		return fmt.Errorf("%w: pending active credential ID", ErrCredentialIdentity)
	}
	if err := r.config.Registration.Activate(ctx, registrationhttp.ActivateRequest{Previous: previous, Active: r.credential}); err != nil {
		return fmt.Errorf("agentruntime: resume credential activation: %w", err)
	}
	return r.state.CompleteCredentialActivation(ctx, r.credential.CredentialID)
}

func (r *Runtime) renewIfDue(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeRequested {
		return ErrClosed
	}
	if !r.credential.RenewalDue(r.config.Now()) {
		return nil
	}
	if r.config.Registration == nil {
		return fmt.Errorf("agentruntime: credential renewal requires registration client")
	}
	previous, previousState := r.credential, r.credentialState
	response, err := r.config.Registration.Renew(ctx, registrationhttp.RenewRequest{Current: previous})
	if err != nil {
		return fmt.Errorf("agentruntime: renew credential: %w", err)
	}
	activeState, err := marshalStateCredential(response.Credential)
	if err != nil {
		return err
	}
	coverage, err := r.coverageBeginsAt()
	if err != nil {
		return err
	}
	_, err = r.state.StageCredentialRenewalAndBind(
		ctx, previousState, activeState, response.Credential.CredentialID,
		response.Archive.ServerIdentityID, response.Archive.Generation,
		response.Archive.AuditArchiveID, coverage, r.config.Now(),
	)
	if err != nil {
		return fmt.Errorf("agentruntime: persist renewed credential: %w", err)
	}
	r.credential, r.credentialState = response.Credential, activeState
	r.heartbeat.setCredentialIdentity(response.Credential.CredentialID, response.Credential.ServerIdentityID)
	if err := r.wal.RebindArchive(response.Archive.AuditArchiveID); err != nil {
		return fmt.Errorf("agentruntime: rebind WAL after renewal: %w", err)
	}
	if err := r.config.Registration.Activate(ctx, registrationhttp.ActivateRequest{Previous: previous, Active: response.Credential}); err != nil {
		return fmt.Errorf("agentruntime: activate renewed credential: %w", err)
	}
	if err := r.state.CompleteCredentialActivation(ctx, response.Credential.CredentialID); err != nil {
		return fmt.Errorf("agentruntime: clear credential activation journal: %w", err)
	}
	return nil
}

func (r *Runtime) installCredentialAndArchive(ctx context.Context, response registrationhttp.CredentialResponse) error {
	stateCredential, err := marshalStateCredential(response.Credential)
	if err != nil {
		return err
	}
	coverage, err := r.coverageBeginsAt()
	if err != nil {
		return err
	}
	if _, err := r.state.InstallCredentialAndBind(ctx, stateCredential,
		response.Archive.ServerIdentityID, response.Archive.Generation,
		response.Archive.AuditArchiveID, coverage, r.config.Now()); err != nil {
		return fmt.Errorf("agentruntime: persist credential/archive: %w", err)
	}
	if err := r.wal.RebindArchive(response.Archive.AuditArchiveID); err != nil {
		return fmt.Errorf("agentruntime: bind WAL archive: %w", err)
	}
	r.credential, r.credentialState = response.Credential, stateCredential
	r.heartbeat.setCredentialIdentity(response.Credential.CredentialID, response.Credential.ServerIdentityID)
	return nil
}

func (r *Runtime) reconcileArchive() error {
	snapshot, err := r.state.Snapshot()
	if err != nil {
		return err
	}
	if snapshot.BoundArchive == nil {
		return fmt.Errorf("agentruntime: credential exists without Archive binding")
	}
	if snapshot.BoundArchive.ServerIdentityID != r.credential.ServerIdentityID {
		return ErrCredentialIdentity
	}
	return r.wal.RebindArchive(snapshot.BoundArchive.ArchiveID)
}

func (r *Runtime) startAuditSync() error {
	snapshot, err := r.state.Snapshot()
	if err != nil {
		return err
	}
	if snapshot.BoundArchive == nil {
		return errors.New("agentruntime: Audit sync requires an Archive binding")
	}
	audit, err := auditsync.NewAgent(auditsync.AgentConfig{WAL: r.wal, ArchiveID: snapshot.BoundArchive.ArchiveID})
	if err != nil {
		return err
	}
	r.handler = &auditHeartbeatHandler{heartbeatHandler: r.heartbeat, audit: audit}
	return nil
}

func (r *Runtime) appendContinuityBoundary(ctx context.Context) error {
	if !r.startup.PreviousUnclean || r.startup.PreviousIncarnation == 0 {
		return nil
	}
	appender, err := auditevents.NewAppender(r.wal)
	if err != nil {
		return err
	}
	var durable *uint64
	if cursor := r.startup.KnownDurableThrough; cursor != nil && cursor.Incarnation == r.startup.PreviousIncarnation {
		value := cursor.Seq
		durable = &value
	}
	if _, err := appender.AppendContinuityUncertain(ctx, r.startup.PreviousIncarnation, durable, r.config.Now()); err != nil {
		return fmt.Errorf("agentruntime: append Audit continuity boundary: %w", err)
	}
	return nil
}

func (r *Runtime) coverageBeginsAt() (agentstate.Cursor, error) {
	bounds, err := r.wal.Bounds()
	if err != nil {
		return agentstate.Cursor{}, err
	}
	cursor := bounds.NextCursor
	if bounds.WALFloor != nil {
		cursor = *bounds.WALFloor
	}
	return agentstate.Cursor{Incarnation: cursor.Incarnation, Seq: cursor.Seq}, nil
}

func (r *Runtime) startDocker(ctx context.Context) error {
	identityState := &identificationState{value: agentsafety.Identification{
		Source: agentsafety.IdentificationFailed, FailClosed: true, Reason: "self-identification has not completed",
	}}
	r.identification = identityState
	docker, err := r.config.DockerOpen(identityState.get)
	if err != nil {
		r.heartbeat.setCapability(producttransport.Capability{ConnectionReady: true, Reason: "Docker unavailable"})
		return nil
	}
	r.docker = docker
	probe, probeErr := docker.Probe(ctx)
	capability := producttransport.Capability{
		ConnectionReady: true, DockerReady: probe.Available,
		DockerAPIVersion:      probe.ServerAPIVersion,
		BundledComposeVersion: r.config.BundledComposeVersion,
		Reason:                probe.Reason,
	}
	if probeErr != nil && capability.Reason == "" {
		capability.Reason = "Docker probe failed"
	}
	if probeErr == nil && probe.Available {
		containers, listErr := docker.List(ctx)
		if listErr != nil {
			capability.DockerReady = false
			capability.Reason = "Docker self-identification inventory unavailable"
		} else {
			identification := agentsafety.IdentifySelf(safetyContainers(containers), r.config.Self)
			identityState.set(identification)
			r.rootAssessments = assessProjectRoots(r.config.ProjectRoots, containers, identification)
			if identification.FailClosed {
				capability.DockerReady = false
				capability.Reason = identification.Reason
			}
		}
	}
	if capability.Reason == "" {
		capability.Reason = "Compose operation handler is not installed"
	}
	applyFilesystemCapability(&capability, r.rootAssessments)
	r.heartbeat.setCapability(capability)
	return nil
}

// applyFilesystemCapability reports the Agent-owned filesystem capability.
// Architecture 3.2 makes each discovery root's identical-path self-check the
// authority: a verified root is readable, and a root demoted for a failed check
// or a read-only mount reports fs_write:false. Architecture 3.1 makes read-only
// operation a first-class mode, so the host-level value states whether any root
// is usable; the authoritative per-root decision stays on each project.
func applyFilesystemCapability(capability *producttransport.Capability, assessments map[string]agentsafety.RootAssessment) {
	var writeReason string
	for _, assessment := range assessments {
		if !assessment.Matched {
			if writeReason == "" {
				writeReason = string(assessment.Reason)
			}
			continue
		}
		capability.FSRead = true
		if assessment.FSWrite {
			capability.FSWrite = true
			return
		}
		writeReason = string(assessment.Reason)
	}
	if !capability.FSRead {
		capability.FSReadReason = "no discovery root passed the identical-path self-check"
	}
	if writeReason == "" {
		writeReason = "no discovery root has a verified writable identical-path Agent mount"
	}
	capability.FSWriteReason = writeReason
}

func assessProjectRoots(roots []string, containers []dockeradapter.Container, identity agentsafety.Identification) map[string]agentsafety.RootAssessment {
	selected := make(map[string]struct{}, len(identity.SelectedAgentIDs))
	for _, id := range identity.SelectedAgentIDs {
		selected[id] = struct{}{}
	}
	mounts := make([]agentsafety.Mount, 0)
	for _, container := range containers {
		if _, ok := selected[container.ID]; !ok {
			continue
		}
		for _, mount := range container.Mounts {
			mounts = append(mounts, agentsafety.Mount{
				Type: mount.Type, Source: mount.Source, Destination: mount.Destination, RW: mount.ReadWrite,
			})
		}
	}
	result := make(map[string]agentsafety.RootAssessment, len(roots))
	for _, root := range roots {
		result[root] = agentsafety.AssessDiscoveryRoot(root, mounts)
	}
	return result
}

func (r *Runtime) startObservedAudit(ctx context.Context) error {
	source, ok := r.docker.(dockerEventSource)
	if !ok {
		return nil
	}
	if err := r.reconcileDockerSnapshot(ctx); err != nil {
		return err
	}
	appender, err := auditevents.NewAppender(r.wal)
	if err != nil {
		return err
	}
	generatorConfig := auditgen.DefaultConfig()
	generatorConfig.Clock = runtimeClock{now: r.config.Now}
	generator, err := auditgen.New(generatorConfig)
	if err != nil {
		return err
	}
	runnerConfig := auditevents.RunnerConfig{
		Source: source, Generator: generator, Appender: appender, Now: r.config.Now,
		Checkpoint: r.state.AdvanceLastDockerEventAt,
	}
	if inspector, available := r.docker.(dockerInspector); available {
		runnerConfig.Inspector = inspector
	}
	runner, err := auditevents.NewRunner(runnerConfig)
	if err != nil {
		return err
	}
	eventCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	r.eventCancel, r.eventDone = cancel, done
	go r.maintainObservedAudit(eventCtx, runner, done)
	return nil
}

func (r *Runtime) maintainObservedAudit(ctx context.Context, runner *auditevents.Runner, done chan<- error) {
	defer close(done)
	for {
		if err := r.reconcileDockerSnapshot(ctx); err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				done <- nil
				return
			case <-timer.C:
				continue
			}
		}
		snapshot, snapshotErr := r.state.Snapshot()
		if snapshotErr != nil {
			done <- snapshotErr
			return
		}
		since := snapshot.LastDockerEventAt
		err := runner.Run(ctx, since)
		if ctx.Err() != nil {
			if errors.Is(err, auditevents.ErrStreamClosed) || errors.Is(err, context.Canceled) {
				err = nil
			}
			done <- err
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			done <- nil
			return
		case <-timer.C:
		}
	}
}

func (r *Runtime) stopObservedAudit() error {
	if r.eventCancel == nil {
		return nil
	}
	r.eventCancel()
	var err error
	for value := range r.eventDone {
		err = errors.Join(err, value)
	}
	return err
}

type runtimeClock struct{ now func() time.Time }

func (clock runtimeClock) Now() time.Time { return clock.now() }

func safetyContainers(values []dockeradapter.Container) []agentsafety.Container {
	result := make([]agentsafety.Container, 0, len(values))
	for _, value := range values {
		name := ""
		if len(value.Names) != 0 {
			name = value.Names[0]
		}
		result = append(result, agentsafety.Container{ID: value.ID, Name: name, Labels: value.Labels})
	}
	return result
}

func credentialMaterialPresent(value agentstate.Credential) bool {
	return value.FileReference != "" || len(value.Data) != 0
}

func loadCredential(value agentstate.Credential) (identity.Credential, error) {
	if value.FileReference != "" {
		return identity.LoadCredential(value.FileReference)
	}
	return identity.ParseCredential(value.Data)
}

func marshalStateCredential(value identity.Credential) (agentstate.Credential, error) {
	payload, err := identity.MarshalCredential(value)
	if err != nil {
		return agentstate.Credential{}, err
	}
	return agentstate.Credential{Data: payload}, nil
}

func validateCredentialIdentity(credential identity.Credential, snapshot agentstate.Snapshot) error {
	if credential.AgentID != snapshot.AgentID {
		return ErrCredentialIdentity
	}
	if snapshot.BoundArchive == nil {
		return fmt.Errorf("%w: missing Archive binding", ErrCredentialIdentity)
	}
	if credential.ServerIdentityID != snapshot.BoundArchive.ServerIdentityID {
		return ErrCredentialIdentity
	}
	return nil
}

func cloneMetadata(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
