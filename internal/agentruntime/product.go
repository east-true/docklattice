package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/east-true/docklattice/internal/agentproduct"
	"github.com/east-true/docklattice/internal/agentprojects"
	"github.com/east-true/docklattice/internal/agentsafety"
	"github.com/east-true/docklattice/internal/agentstorage"
	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditgen"
	"github.com/east-true/docklattice/internal/backup"
	"github.com/east-true/docklattice/internal/composeconfig"
	"github.com/east-true/docklattice/internal/composeexec"
	"github.com/east-true/docklattice/internal/composesource"
	productconfig "github.com/east-true/docklattice/internal/config"
	"github.com/east-true/docklattice/internal/discovery"
	"github.com/east-true/docklattice/internal/dockeradapter"
	"github.com/east-true/docklattice/internal/managedaudit"
	"github.com/east-true/docklattice/internal/operation"
)

// startProduct is intentionally tied to the production Docker adapter. Unit
// boot fakes exercise the control lifecycle without pretending to implement
// Docker streams or mutations; the executable always opens this adapter.
func (r *Runtime) startProduct(ctx context.Context) error {
	docker, production := r.docker.(*dockeradapter.Adapter)
	if !production {
		return nil
	}
	stateRoot := commonStateRoot(r.config.StateDir, r.config.WALDir)
	storage, err := agentstorage.New(agentstorage.Config{
		StateRoot: stateRoot, Budget: r.config.storageBudget, Observe: r.config.storageObserve,
	})
	if err != nil {
		return err
	}
	journal, err := operation.NewFileJournal(stateRoot, storage)
	if err != nil {
		return err
	}
	engineConfig := operation.DefaultConfig()
	engineConfig.Clock = runtimeClock{now: r.config.Now}
	engineConfig.Journal = journal
	appender, err := auditevents.NewAppender(r.wal)
	if err != nil {
		return err
	}
	terminalAuditor, err := managedaudit.New(appender)
	if err != nil {
		return err
	}
	engineConfig.TerminalAuditor = terminalAuditor
	engine, err := operation.New(engineConfig)
	if err != nil {
		return err
	}
	backups, err := backup.New(stateRoot, storage, nil)
	if err != nil {
		return err
	}
	scanner, err := discovery.New(discovery.DefaultConfig(r.config.ProjectRoots...))
	if err != nil {
		return err
	}
	executionPolicy := func(root string) (bool, string) {
		assessment, ok := r.rootAssessments[root]
		if !ok || !assessment.ComposeExec {
			if ok {
				return false, string(assessment.Reason)
			}
			return false, "discovery root has no verified identical-path Agent mount"
		}
		return true, string(assessment.Reason)
	}
	writePolicy := func(root string) (bool, string) {
		assessment, ok := r.rootAssessments[root]
		if !ok || !assessment.FSWrite {
			if ok {
				return false, string(assessment.Reason)
			}
			return false, "discovery root has no verified writable identical-path Agent mount"
		}
		return true, string(assessment.Reason)
	}
	evaluator := r.config.projectEvaluator
	if evaluator == nil {
		evaluator = composeconfig.Evaluator{}
	}
	catalog, err := agentprojects.NewWithSourceGraph(r.startup.AgentID, scanner, evaluator, composesource.New(), executionPolicy, writePolicy)
	if err != nil {
		return err
	}
	initialDiscoveryErr := catalog.Rescan(ctx)
	if initialDiscoveryErr != nil && !agentprojects.IsPublishedDegraded(initialDiscoveryErr) {
		return fmt.Errorf("agentruntime: initial project discovery: %w", initialDiscoveryErr)
	}
	if initialDiscoveryErr != nil {
		r.diagnostics.problem("discovery_scan_failed", initialDiscoveryErr)
	}
	if _, err := backups.Recover(ctx, catalog); err != nil {
		return fmt.Errorf("agentruntime: recover backup restore journal: %w", err)
	}
	staging, err := agentstorage.NewProjectStagingReclaimer(catalog)
	if err != nil {
		return err
	}
	eviction, err := agentstorage.NewEvictionExecutor(agentstorage.EvictionConfig{
		WAL: r.wal, Operations: engine, Backups: backups, FileStaging: staging,
	})
	if err != nil {
		return err
	}
	runtimeStorage, err := newRuntimeStorage(storage, eviction, r.configTimeouts().DiscoveryRescan)
	if err != nil {
		return err
	}
	if _, err := runtimeStorage.reclaimUntilStable(ctx); err != nil {
		return fmt.Errorf("agentruntime: boot storage reclaim: %w", err)
	}
	files, err := agentproduct.NewProjectFiles(catalog)
	if err != nil {
		return err
	}
	logs, err := docker.LogRelaySource()
	if err != nil {
		return err
	}
	stats, err := docker.LiveStatsSource()
	if err != nil {
		return err
	}
	compose := protectedCompose{runner: composeexec.Runner{CancelGrace: productconfig.V1Defaults().CancelGracePeriod}, identity: r.identification}
	product, err := agentproduct.New(agentproduct.Config{
		Control: r.handler, Docker: docker, Projects: catalog, Files: files, Backups: backups,
		Engine: engine, Compose: compose, Admission: runtimeStorage,
		Timeouts: r.configTimeouts(), LogSource: logs, StatsSource: stats,
		StatsSampleInterval: productconfig.V1Defaults().StatsSampleInterval,
		MatrixDocker:        docker,
		MatrixPaths:         managedPaths(r.config.ProjectRoots, stateRoot),
		MatrixFrameInterval: productconfig.V1Defaults().MetricsFrameInterval,
	})
	if err != nil {
		return err
	}
	r.handler, r.productCloser = product, product
	r.metricsMatrixInstalled = true
	r.operationEngine = engine
	r.productStorage = runtimeStorage
	r.startDiscoveryLoop(ctx, catalog, runtimeStorage, appender, r.config.DiscoveryInterval)
	// A safely published partial scan degrades Discovery through ScanStatus and
	// per-Project capability reasons. It must not disable Docker, Logs, Metrics,
	// or Compose operations for Projects that were individually verified.
	r.updateProductCapability(composeDiscoveryCapabilityError(initialDiscoveryErr, catalog), nil)
	return nil
}

// managedPaths are the paths DockLattice writes to on this host: the discovery
// roots and the Agent state root. They are what the host row reports capacity
// for. Deduplication is by filesystem and happens at probe time, because two
// distinct paths on one mount are one filesystem and neither of them is
// redundant as a path.
func managedPaths(roots []string, stateRoot string) []string {
	paths := make([]string, 0, len(roots)+1)
	paths = append(paths, roots...)
	if stateRoot != "" {
		paths = append(paths, stateRoot)
	}
	return paths
}

func (r *Runtime) startDiscoveryLoop(ctx context.Context, catalog *agentprojects.Catalog, storage *runtimeStorage, appender *auditevents.Appender, interval time.Duration) {
	discoveryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	r.discoveryCancel, r.discoveryDone = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-discoveryCtx.Done():
				return
			case <-ticker.C:
				discoveryErr := catalog.RescanForExternalChanges(discoveryCtx, func(auditCtx context.Context, changes []agentprojects.ExternalConfigChange) error {
					return appendExternalConfigChangeAudit(auditCtx, appender, changes)
				})
				capabilityDiscoveryErr := discoveryErr
				if discoveryErr != nil {
					r.diagnostics.problem("discovery_scan_failed", discoveryErr)
					capabilityDiscoveryErr = composeDiscoveryCapabilityError(discoveryErr, catalog)
				} else {
					r.diagnostics.resolved("discovery_scan_failed")
				}
				_, storageErr := storage.reclaimUntilStable(discoveryCtx)
				r.updateProductCapability(capabilityDiscoveryErr, storageErr)
			}
		}
	}()
}

func composeDiscoveryCapabilityError(err error, catalog *agentprojects.Catalog) error {
	if err == nil || !agentprojects.IsPublishedDegraded(err) {
		return err
	}
	projects, _ := catalog.Snapshot()
	for _, project := range projects {
		if !project.Stale && project.ComposeExecutable {
			return nil
		}
	}
	return err
}

// appendExternalConfigChangeAudit emits one content-free, bounded observation
// for a periodic scan. The Server's project mirror supplies the per-project
// drift badge from its existing current/applied fingerprint comparison; Audit
// therefore does not persist project paths, file names, hashes, or contents.
func appendExternalConfigChangeAudit(ctx context.Context, appender *auditevents.Appender, changes []agentprojects.ExternalConfigChange) error {
	if appender == nil {
		return auditevents.ErrAppenderUnavailable
	}
	if len(changes) == 0 {
		return nil
	}
	observedAt := changes[0].ObservedAt.UTC()
	if observedAt.IsZero() {
		return errors.New("agentruntime: external configuration observation time is required")
	}
	var changedFiles uint64
	for _, change := range changes {
		if change.ProjectUID == "" || change.ChangedFileCount <= 0 || !change.ObservedAt.UTC().Equal(observedAt) {
			return errors.New("agentruntime: invalid external configuration change")
		}
		changedFiles += uint64(change.ChangedFileCount)
	}
	_, err := appender.Append(ctx, auditgen.Event{
		Kind: auditgen.KindObserved, ResourceType: "agent", ResourceID: "discovery", Action: "external_config_change",
		FirstAt: observedAt, LastAt: observedAt, Count: uint64(len(changes)),
		Attributes: map[string]string{
			"changed_project_count": strconv.Itoa(len(changes)),
			"changed_file_count":    strconv.FormatUint(changedFiles, 10),
		},
	})
	if err != nil {
		return fmt.Errorf("agentruntime: append external configuration Audit: %w", err)
	}
	return nil
}

func (r *Runtime) updateProductCapability(discoveryErr, reclaimErr error) {
	r.heartbeat.mu.Lock()
	defer r.heartbeat.mu.Unlock()
	capability := r.heartbeat.capability
	if !capability.DockerReady {
		r.heartbeat.capability = capability
		return
	}
	// Live metrics need Docker and nothing else. They are deliberately settled
	// before the Compose and storage checks below, each of which returns early:
	// a host whose Compose roots are unusable can still be watched.
	capability.MetricsMatrix = r.metricsMatrixInstalled
	if discoveryErr != nil {
		capability.ComposeReady = false
		capability.Reason = "project discovery failed"
		r.heartbeat.capability = capability
		return
	}
	if !anyComposeRoot(r.rootAssessments) {
		capability.ComposeReady = false
		capability.Reason = "no verified Compose discovery root"
		r.heartbeat.capability = capability
		return
	}
	status, storedErr := r.productStorage.snapshot()
	if reclaimErr != nil || storedErr != nil {
		capability.ComposeReady = false
		capability.Reason = "storage reclaim failed"
	} else {
		capability.ComposeReady = true
		if status.AfterState.Degraded {
			capability.Reason = fmt.Sprintf("DEGRADED_STORAGE: %s", status.AfterState.Reason)
		} else {
			capability.Reason = ""
		}
	}
	r.heartbeat.capability = capability
}

func (r *Runtime) stopDiscovery() {
	if r.discoveryCancel == nil {
		return
	}
	r.discoveryCancel()
	<-r.discoveryDone
}

func (r *Runtime) configTimeouts() productconfig.OperationTimeouts {
	return productconfig.V1Defaults().OperationTimeout
}

func commonStateRoot(stateDir, walDir string) string {
	stateParent, walParent := filepath.Dir(stateDir), filepath.Dir(walDir)
	if stateParent == walParent {
		return stateParent
	}
	return stateDir
}

func anyComposeRoot(assessments map[string]agentsafety.RootAssessment) bool {
	for _, assessment := range assessments {
		if assessment.ComposeExec {
			return true
		}
	}
	return false
}

type protectedCompose struct {
	runner   composeexec.Runner
	identity *identificationState
}

func (compose protectedCompose) Run(ctx context.Context, spec composeexec.Spec, output chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	if compose.identity == nil {
		return composeexec.Result{}, errors.New("agentruntime: Compose self identity unavailable")
	}
	action := agentsafety.ActionComposeMutation
	switch spec.Operation {
	case composeexec.OperationPS, composeexec.OperationConfig:
		action = agentsafety.ActionQuery
	case composeexec.OperationLogs:
		action = agentsafety.ActionLogs
	case composeexec.OperationDown:
		action = agentsafety.ActionComposeDown
	}
	decision := agentsafety.Decide(compose.identity.get(), agentsafety.Action{Kind: action, ComposeProject: spec.Project.Name})
	if !decision.Allowed {
		return composeexec.Result{}, fmt.Errorf("agentruntime: Compose denied: %s: %s", decision.Code, decision.Reason)
	}
	return compose.runner.Run(ctx, spec, output)
}
