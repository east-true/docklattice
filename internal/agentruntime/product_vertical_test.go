package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/east-true/dockpilot/internal/agentstorage"
	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeconfig"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/operation"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

const (
	verticalAgentID    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	verticalWorkloadID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// verticalMobyEngine implements the production Adapter's Engine, events,
// logs, and stats boundaries. The test intentionally exercises only bounded
// container.list; streaming methods exist so startProduct must prove that the
// real Adapter can construct every live product handler without spawning a
// Compose subprocess.
type verticalMobyEngine struct {
	mu           sync.Mutex
	listCalls    int
	closed       bool
	projectRoot  string
	projectRoots []string
}

func (*verticalMobyEngine) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	return client.PingResult{APIVersion: "1.55"}, nil
}

func (*verticalMobyEngine) ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
	return client.ServerVersionResult{Version: "29.0.0", APIVersion: "1.55"}, nil
}

func (engine *verticalMobyEngine) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	engine.mu.Lock()
	engine.listCalls++
	projectRoot := engine.projectRoot
	projectRoots := append([]string(nil), engine.projectRoots...)
	engine.mu.Unlock()
	var mounts []container.MountPoint
	if projectRoot != "" {
		mounts = []container.MountPoint{{Type: "bind", Source: projectRoot, Destination: projectRoot, RW: true}}
	}
	for _, root := range projectRoots {
		mounts = append(mounts, container.MountPoint{Type: "bind", Source: root, Destination: root, RW: true})
	}
	return client.ContainerListResult{Items: []container.Summary{
		{
			ID: verticalAgentID, Names: []string{"/dockpilot-agent"}, Image: "dockpilot:test", State: container.StateRunning,
			Labels: map[string]string{agentsafety.AgentRoleLabel: agentsafety.AgentRoleValue, agentsafety.ComposeProjectLabel: "dockpilot"},
			Mounts: mounts,
		},
		{ID: verticalWorkloadID, Names: []string{"/workload"}, Image: "workload:test", State: container.StateRunning},
	}}, nil
}

func (*verticalMobyEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: id, Config: &container.Config{}}}, nil
}

func (*verticalMobyEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}

func (*verticalMobyEngine) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, nil
}

func (*verticalMobyEngine) ContainerRestart(context.Context, string, client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	return client.ContainerRestartResult{}, nil
}

func (*verticalMobyEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, nil
}

func (*verticalMobyEngine) ContainerStats(context.Context, string, client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	return client.ContainerStatsResult{Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (*verticalMobyEngine) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (*verticalMobyEngine) Events(ctx context.Context, _ client.EventsListOptions) client.EventsResult {
	messages := make(chan events.Message)
	errorsOut := make(chan error)
	go func() {
		<-ctx.Done()
		close(messages)
		close(errorsOut)
	}()
	return client.EventsResult{Messages: messages, Err: errorsOut}
}

func (engine *verticalMobyEngine) Close() error {
	engine.mu.Lock()
	engine.closed = true
	engine.mu.Unlock()
	return nil
}

func TestProductionAdapterBootAssemblesCompleteProductHandler(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	engine := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(engine, identity, dockeradapter.MinimumAPIVersion)
	}

	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	if _, ok := runtime.docker.(*dockeradapter.Adapter); !ok {
		t.Fatalf("Boot Docker = %T, want production Adapter", runtime.docker)
	}
	if runtime.productStorage == nil || runtime.productStorage.controller == nil || runtime.productStorage.executor == nil {
		t.Fatal("Boot did not assemble production storage controller and eviction executor")
	}
	if _, ok := runtime.handler.(producttransport.QueryHandler); !ok {
		t.Fatal("production handler does not implement Query")
	}
	if _, ok := runtime.handler.(producttransport.OperationHandler); !ok {
		t.Fatal("production handler does not implement Operation")
	}
	if _, ok := runtime.handler.(producttransport.LogStreamHandler); !ok {
		t.Fatal("production handler does not implement Logs")
	}
	if _, ok := runtime.handler.(producttransport.StatsStreamHandler); !ok {
		t.Fatal("production handler does not implement Stats")
	}
	if _, ok := runtime.handler.(producttransport.AuditSyncHandler); !ok {
		t.Fatal("production handler does not preserve Audit Sync")
	}

	query := runtime.handler.(producttransport.QueryHandler)
	result, err := query.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: "container.list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Payload), verticalAgentID) || !strings.Contains(string(result.Payload), verticalWorkloadID) {
		t.Fatalf("container.list payload = %s", result.Payload)
	}

	capability, err := runtime.handler.Heartbeat(context.Background(), producttransport.SessionInfo{
		AgentID: runtime.startup.AgentID, Incarnation: runtime.startup.CurrentIncarnation,
		CredentialID: runtime.credential.CredentialID, ServerIdentityID: runtime.credential.ServerIdentityID,
	}, server.now)
	if err != nil {
		t.Fatal(err)
	}
	if !capability.ConnectionReady || !capability.DockerReady || capability.ComposeReady || capability.Reason == "" {
		t.Fatalf("production capability without roots = %+v", capability)
	}

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	listCalls, closed := engine.listCalls, engine.closed
	engine.mu.Unlock()
	if listCalls < 2 {
		t.Fatalf("Moby ContainerList calls = %d, want boot inventory plus live query", listCalls)
	}
	if !closed {
		t.Fatal("Runtime.Close did not close production Moby Engine")
	}
}

func TestProductionBootReclaimsStateRootPressure(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	operationsDir := filepath.Join(root, "operations")
	if err := os.Mkdir(operationsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(operationsDir, ".orphan.tmp")
	if err := os.WriteFile(temporary, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	config.storageBudget = verticalStorageBudget()
	config.storageObserve = observationWhileFileExists(temporary)
	moby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}

	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned operation temp survived boot reclaim: %v", err)
	}
	status, storageErr := runtime.productStorage.snapshot()
	if storageErr != nil || status.AfterState.Degraded {
		t.Fatalf("boot storage result=%+v err=%v", status, storageErr)
	}
}

func TestProductionAdmissionGetsOneSafeReclaimAttempt(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	config.storageBudget = verticalStorageBudget()
	temporary := filepath.Join(root, "operations", ".admission.tmp")
	var pressureObservations atomic.Int64
	config.storageObserve = func(_ context.Context, _ string) (diskbudget.Observation, error) {
		if _, err := os.Lstat(temporary); err == nil {
			pressureObservations.Add(1)
			return verticalPressureObservation(), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return diskbudget.Observation{}, err
		}
		return verticalHealthyObservation(), nil
	}
	moby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	if err := os.WriteFile(temporary, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.productStorage.AdmitOperation(context.Background(), diskbudget.OperationBackupCreate); err != nil {
		t.Fatalf("admission after reclaim: %v", err)
	}
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admission reclaim left temp: %v", err)
	}
	if got := pressureObservations.Load(); got != 2 {
		// Initial admission and the reclaimer's before-observation see pressure;
		// after-observation and final admission see the cleaned filesystem.
		t.Fatalf("pressure observations = %d, want exactly one reclaim pass (2)", got)
	}
}

func TestProductionPersistentExternalFreeSpacePressureStaysDegraded(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = []string{projectRoot}
	config.storageBudget = verticalStorageBudget()
	config.storageObserve = func(context.Context, string) (diskbudget.Observation, error) {
		return verticalPressureObservation(), nil
	}
	moby := &verticalMobyEngine{projectRoot: projectRoot}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	capability, err := runtime.handler.Heartbeat(context.Background(), producttransport.SessionInfo{
		AgentID: runtime.startup.AgentID, Incarnation: runtime.startup.CurrentIncarnation,
		CredentialID: runtime.credential.CredentialID, ServerIdentityID: runtime.credential.ServerIdentityID,
	}, server.now)
	if err != nil {
		t.Fatal(err)
	}
	if !capability.ComposeReady || !strings.Contains(capability.Reason, "DEGRADED_STORAGE: FILESYSTEM_FREE_LOW") {
		t.Fatalf("persistent pressure capability = %+v", capability)
	}
	if err := runtime.productStorage.AdmitOperation(context.Background(), diskbudget.OperationBackupCreate); !errors.Is(err, agentstorage.ErrAdmission) {
		t.Fatalf("persistent pressure admission error = %v", err)
	}
}

func TestProductionStorageDiscoveryTickStopsWithRuntime(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	config.DiscoveryInterval = 5 * time.Millisecond
	config.storageBudget = verticalStorageBudget()
	var observations atomic.Int64
	config.storageObserve = func(context.Context, string) (diskbudget.Observation, error) {
		observations.Add(1)
		return verticalHealthyObservation(), nil
	}
	moby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for observations.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := observations.Load(); got < 3 {
		t.Fatalf("periodic storage observations = %d", got)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterClose := observations.Load()
	time.Sleep(20 * time.Millisecond)
	if got := observations.Load(); got != afterClose {
		t.Fatalf("storage observations continued after Close: before=%d after=%d", afterClose, got)
	}
}

func TestProductionPeriodicDiscoveryAuditsExternalConfigChange(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(projectRoot, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: initial-private-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = []string{projectRoot}
	config.DiscoveryInterval = 10 * time.Millisecond
	config.projectEvaluator = verticalProjectEvaluator{}
	moby := &verticalMobyEngine{projectRoot: projectRoot}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())

	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: externally-changed-private-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The rescan interval is 10ms; the bound only has to outlast scheduling
	// noise on a loaded machine, and the test still fails if the Audit never
	// appears.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, readErr := runtime.WAL().ReadAuditFrom(context.Background(), auditwal.Cursor{
			Incarnation: runtime.Startup().CurrentIncarnation, Seq: 1,
		}, 32)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, record := range result.Records {
			envelope, decodeErr := auditevents.Decode(record.Payload)
			if decodeErr != nil || envelope.Event.Action != "external_config_change" {
				continue
			}
			if envelope.Event.Kind != auditgen.KindObserved || envelope.Event.Actor != "" ||
				envelope.Event.ResourceType != "agent" || envelope.Event.ResourceID != "discovery" ||
				envelope.Event.Count != 1 || envelope.Event.Attributes["changed_project_count"] != "1" ||
				envelope.Event.Attributes["changed_file_count"] != "1" {
				t.Fatalf("external config Audit=%+v", envelope)
			}
			if strings.Contains(string(record.Payload), "externally-changed-private-value") ||
				strings.Contains(string(record.Payload), composePath) {
				t.Fatalf("external config Audit leaked file content or path: %s", record.Payload)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("periodic discovery did not append external configuration Audit")
}

type verticalProjectEvaluator struct{}

func (verticalProjectEvaluator) Evaluate(_ context.Context, workingDir string, files []string) (composeconfig.Result, error) {
	return composeconfig.Result{Project: composeexec.Project{
		WorkingDir: workingDir, Files: append([]string(nil), files...), Name: filepath.Base(workingDir),
	}, Services: []string{"app"}}, nil
}

type verticalRestoreJournal struct {
	Version              int                   `json:"version"`
	OperationID          string                `json:"operation_id"`
	ProjectUID           string                `json:"project_uid"`
	BackupID             string                `json:"backup_id"`
	WorkingDir           string                `json:"working_dir"`
	Phase                string                `json:"phase"`
	PreRestoreSnapshotID string                `json:"pre_restore_snapshot_id"`
	Files                []verticalJournalFile `json:"files"`
}

type verticalJournalFile struct {
	Target          string `json:"target"`
	StagedPath      string `json:"staged_path"`
	Status          string `json:"status"`
	OriginalExisted bool   `json:"original_existed"`
}

func TestProductionBootRecoversBackupJournalsAndKeepsProjectFailuresIsolated(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	firstMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(firstMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	agentID := first.Startup().AgentID
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	projectBase := filepath.Join(root, "projects")
	if err := os.Mkdir(projectBase, 0o700); err != nil {
		t.Fatal(err)
	}
	projectDirs := map[string]string{}
	for _, name := range []string{"preparing", "partial", "blocked", "normal"} {
		directory := filepath.Join(projectBase, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		projectDirs[name] = directory
	}
	project := func(name string) backup.Project {
		uid, uidErr := projectmodel.UID(agentID, projectDirs[name])
		if uidErr != nil {
			t.Fatal(uidErr)
		}
		return backup.Project{UID: uid, Name: name, WorkingDir: projectDirs[name]}
	}
	manager, err := backup.New(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	createSnapshot := func(p backup.Project, operationID string) backup.Backup {
		created, createErr := manager.Create(context.Background(), backup.CreateRequest{
			Project: p, RelativePaths: []string{"compose.yaml"}, Trigger: backup.TriggerPreRestore,
			OperationID: operationID, CreatedAt: server.now,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return created
	}

	preparing := project("preparing")
	preparingBackup := createSnapshot(preparing, "snapshot-preparing")
	preparingStage := ".dockpilot-restore-preparing.tmp"
	if err := os.WriteFile(filepath.Join(preparing.WorkingDir, preparingStage), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerticalRestoreJournal(t, root, verticalRestoreJournal{
		Version: 1, OperationID: "crash-preparing", ProjectUID: preparing.UID,
		BackupID: preparingBackup.Manifest.BackupID, WorkingDir: preparing.WorkingDir, Phase: "PREPARING",
		PreRestoreSnapshotID: preparingBackup.Manifest.BackupID,
		Files:                []verticalJournalFile{{Target: "compose.yaml", StagedPath: preparingStage, Status: "pending", OriginalExisted: true}},
	})

	partial := project("partial")
	partialBackup := createSnapshot(partial, "snapshot-partial")
	if err := os.WriteFile(filepath.Join(partial.WorkingDir, "compose.yaml"), []byte("partially restored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	partialStage := ".dockpilot-restore-partial.tmp"
	if err := os.WriteFile(filepath.Join(partial.WorkingDir, partialStage), []byte("remaining staged data"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerticalRestoreJournal(t, root, verticalRestoreJournal{
		Version: 1, OperationID: "crash-partial", ProjectUID: partial.UID,
		BackupID: partialBackup.Manifest.BackupID, WorkingDir: partial.WorkingDir, Phase: "COMMITTING",
		PreRestoreSnapshotID: partialBackup.Manifest.BackupID,
		Files:                []verticalJournalFile{{Target: "compose.yaml", StagedPath: partialStage, Status: "replaced", OriginalExisted: true}},
	})

	blocked := project("blocked")
	blockedBackup := createSnapshot(blocked, "snapshot-blocked")
	writeVerticalRestoreJournal(t, root, verticalRestoreJournal{
		Version: 1, OperationID: "crash-mismatch", ProjectUID: blocked.UID,
		BackupID: blockedBackup.Manifest.BackupID, WorkingDir: filepath.Join(projectBase, "wrong-directory"), Phase: "COMMITTING",
		PreRestoreSnapshotID: blockedBackup.Manifest.BackupID,
	})

	operationJournal, err := operation.NewFileJournal(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	operationConfig := operation.DefaultConfig()
	operationConfig.Journal = operationJournal
	crashedEngine, err := operation.New(operationConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id         string
		project    backup.Project
		backupID   string
		committing bool
	}{
		{id: "crash-preparing", project: preparing, backupID: preparingBackup.Manifest.BackupID},
		{id: "crash-partial", project: partial, backupID: partialBackup.Manifest.BackupID, committing: true},
		{id: "crash-mismatch", project: blocked, backupID: blockedBackup.Manifest.BackupID, committing: true},
	} {
		op, created, createErr := crashedEngine.Create(context.Background(), operation.Spec{
			OperationID: fixture.id, Type: operation.TypeBackupRestore, ProjectKey: fixture.project.UID, Target: fixture.backupID,
		})
		if createErr != nil || !created {
			t.Fatalf("create crash operation %s: created=%v err=%v", fixture.id, created, createErr)
		}
		for _, status := range []operation.Status{operation.StatusDispatched, operation.StatusRunning} {
			if err := op.TransitionStatus(status, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := op.AdvancePhase(operation.PhaseExecuting); err != nil {
			t.Fatal(err)
		}
		if fixture.committing {
			if err := op.EnterCommit(); err != nil {
				t.Fatal(err)
			}
		}
	}

	config.JoinToken = ""
	config.ProjectRoots = []string{preparing.WorkingDir, partial.WorkingDir, blocked.WorkingDir, projectDirs["normal"]}
	config.projectEvaluator = verticalProjectEvaluator{}
	secondMoby := &verticalMobyEngine{projectRoots: append([]string(nil), config.ProjectRoots...)}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(secondMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	restarted, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())

	if _, err := os.Lstat(filepath.Join(preparing.WorkingDir, preparingStage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PREPARING staging survived recovery: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(partial.WorkingDir, "compose.yaml")); err != nil || string(got) != "services: {}\n" {
		t.Fatalf("partial restore rollback contents=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(partial.WorkingDir, partialStage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("COMMITTING staging survived rollback: %v", err)
	}
	for _, id := range []string{"crash-preparing", "crash-partial"} {
		if _, err := os.Lstat(filepath.Join(root, "restore-journal", id+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("durably recovered journal %s remains: %v", id, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "restore-journal", "crash-mismatch.json")); err != nil {
		t.Fatalf("blocked mismatch journal was lost: %v", err)
	}
	recoveredEngine := restarted.operationEngine.(*operation.Engine)
	for _, expectation := range []struct {
		id      string
		partial bool
	}{
		{id: "crash-preparing", partial: false},
		{id: "crash-partial", partial: true},
		{id: "crash-mismatch", partial: true},
	} {
		record, ok := recoveredEngine.Get(expectation.id)
		if !ok || record.Status != operation.StatusInterrupted || record.ManagedAuditDelivery != operation.ManagedAuditDelivered ||
			record.PartialEffectsPossible != true || (record.Phase == operation.PhaseCommitting) != expectation.partial {
			t.Fatalf("recovered operation %s = %#v ok=%v", expectation.id, record, ok)
		}
	}

	operations := restarted.handler.(producttransport.OperationHandler)
	if _, err := operations.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "blocked-restore", Type: string(operation.TypeBackupRestore), ProjectKey: blocked.UID,
		Target: blockedBackup.Manifest.BackupID, Payload: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatalf("durable acceptance of blocked restore: %v", err)
	}
	blockedRestore := waitVerticalTerminal(t, recoveredEngine, "blocked-restore")
	if blockedRestore.Status != operation.StatusFailed || !strings.Contains(blockedRestore.Error, backup.ErrProjectRecoveryBlocked.Error()) {
		t.Fatalf("blocked restore result = %#v", blockedRestore)
	}
	blockedContents, err := os.ReadFile(filepath.Join(blocked.WorkingDir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(blockedContents)
	payload, err := json.Marshal(map[string]any{
		"version": 1, "expected_sha256": hex.EncodeToString(digest[:]), "content": "services:\n  changed: {}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "blocked-write", Type: string(operation.TypeComposeFileWrite), ProjectKey: blocked.UID,
		Target: "compose.yaml", Payload: payload,
	}); err != nil {
		t.Fatalf("durable acceptance of blocked write: %v", err)
	}
	blockedWrite := waitVerticalTerminal(t, recoveredEngine, "blocked-write")
	if blockedWrite.Status != operation.StatusFailed || !strings.Contains(blockedWrite.Error, backup.ErrProjectRecoveryBlocked.Error()) {
		t.Fatalf("blocked write result = %#v", blockedWrite)
	}

	normal := project("normal")
	if _, err := operations.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "normal-backup", Type: string(operation.TypeBackupCreate), ProjectKey: normal.UID,
		Payload: []byte(`{"version":1,"relative_paths":["compose.yaml"]}`),
	}); err != nil {
		t.Fatalf("normal project operation: %v", err)
	}
	if record := waitVerticalTerminal(t, recoveredEngine, "normal-backup"); record.Status != operation.StatusSuccess {
		t.Fatalf("normal project was affected by isolated block: %#v", record)
	}

	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondMoby.mu.Lock()
	closed := secondMoby.closed
	secondMoby.mu.Unlock()
	if !closed {
		t.Fatal("shutdown did not close production Docker after recovery")
	}
	thirdMoby := &verticalMobyEngine{projectRoots: append([]string(nil), config.ProjectRoots...)}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(thirdMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	third, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("recovery shutdown leaked a durable resource: %v", err)
	}
	if err := third.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProductionBootFailsClosedOnCorruptBackupRecoveryJournalAndReleasesResources(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	firstMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(firstMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(root, "restore-journal", "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.JoinToken = ""
	failedMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(failedMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	failed, err := Boot(context.Background(), config)
	if failed != nil || !errors.Is(err, backup.ErrRecoveryRequired) {
		if failed != nil {
			_ = failed.Close(context.Background())
		}
		t.Fatalf("corrupt recovery Boot = runtime %v error %v", failed != nil, err)
	}
	failedMoby.mu.Lock()
	closed := failedMoby.closed
	failedMoby.mu.Unlock()
	if !closed {
		t.Fatal("failed recovery boot leaked production Docker")
	}
	if err := os.Remove(corrupt); err != nil {
		t.Fatal(err)
	}
	retryMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(retryMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	retry, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("failed recovery boot leaked state/WAL resources: %v", err)
	}
	if err := retry.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func writeVerticalRestoreJournal(t *testing.T, stateRoot string, journal verticalRestoreJournal) {
	t.Helper()
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(stateRoot, "restore-journal")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(directory, journal.OperationID+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitVerticalTerminal(t *testing.T, engine *operation.Engine, operationID string) operation.Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if record, ok := engine.Get(operationID); ok && record.Status.Terminal() {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	record, _ := engine.Get(operationID)
	t.Fatalf("operation %q did not terminate: %#v", operationID, record)
	return operation.Record{}
}

func verticalStorageBudget() diskbudget.Config {
	return diskbudget.Config{
		StateBudgetBytes: 1000, EntryFreeBytes: 100, EntryFreePercent: 5,
		ExitFreeBytes: 120, ExitFreePercent: 6, ExitStatePercent: 90, EmergencyReserveBytes: 50,
	}
}

func verticalPressureObservation() diskbudget.Observation {
	return diskbudget.Observation{FilesystemTotalBytes: 10_000, FilesystemFreeBytes: 50, AgentStateBytes: 100}
}

func verticalHealthyObservation() diskbudget.Observation {
	return diskbudget.Observation{FilesystemTotalBytes: 10_000, FilesystemFreeBytes: 1000, AgentStateBytes: 100}
}

func observationWhileFileExists(path string) agentstorage.Observer {
	return func(context.Context, string) (diskbudget.Observation, error) {
		if _, err := os.Lstat(path); err == nil {
			return verticalPressureObservation(), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return diskbudget.Observation{}, err
		}
		return verticalHealthyObservation(), nil
	}
}

func TestProductionAdapterMissingStreamingAPIFailsBoot(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	base := &verticalMobyEngineWithoutStreams{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(base, identity, dockeradapter.MinimumAPIVersion)
	}

	runtime, err := Boot(context.Background(), config)
	if runtime != nil || err == nil {
		if runtime != nil {
			_ = runtime.Close(context.Background())
		}
		t.Fatalf("Boot without Moby streaming APIs = runtime %v, error %v", runtime != nil, err)
	}
	if !errors.Is(err, dockeradapter.ErrUnavailable) && !strings.Contains(err.Error(), "stats streaming") && !strings.Contains(err.Error(), "log streaming") {
		t.Fatalf("missing stream failure = %v", err)
	}
}

func TestRuntimeCloseCanRetryAfterBoundedEngineShutdown(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	moby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(moby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	engine, ok := runtime.operationEngine.(*operation.Engine)
	if !ok {
		t.Fatalf("operation Engine = %T", runtime.operationEngine)
	}
	started := make(chan struct{})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	runnerDone := make(chan struct{})
	_, created, err := engine.StartOperation(context.Background(), operation.Spec{
		OperationID: "runtime-close-retry", Type: operation.TypeComposePull, ProjectKey: "project",
	}, func(ctx context.Context, current *operation.Operation) {
		defer close(runnerDone)
		if err := current.TransitionStatus(operation.StatusRunning, "", ""); err != nil {
			t.Errorf("running: %v", err)
			return
		}
		if err := current.AdvancePhase(operation.PhaseExecuting); err != nil {
			t.Errorf("executing: %v", err)
			return
		}
		close(started)
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		if err := current.TransitionStatus(operation.StatusCanceled, "", context.Cause(ctx).Error()); err != nil {
			t.Errorf("canceled: %v", err)
		}
	})
	if err != nil || !created {
		t.Fatalf("StartOperation created=%v err=%v", created, err)
	}
	<-started

	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = runtime.Close(short)
	cancelShort()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v", err)
	}
	<-cleanupStarted
	moby.mu.Lock()
	closedAfterTimeout := moby.closed
	moby.mu.Unlock()
	if closedAfterTimeout {
		t.Fatal("first Close closed Docker while operation runner was still alive")
	}
	if _, err := runtime.wal.Bounds(); err != nil {
		t.Fatalf("first Close closed WAL while operation runner was still alive: %v", err)
	}

	close(releaseCleanup)
	<-runnerDone
	second, cancelSecond := context.WithTimeout(context.Background(), 2*time.Second)
	err = runtime.Close(second)
	cancelSecond()
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}
	moby.mu.Lock()
	closedAfterRetry := moby.closed
	moby.mu.Unlock()
	if !closedAfterRetry {
		t.Fatal("retrying Close did not close Docker")
	}
	record, ok := engine.Get("runtime-close-retry")
	if !ok || record.Status != operation.StatusCanceled || record.CancelReason != operation.CancelReasonAgentShutdown ||
		record.ManagedAuditDelivery != operation.ManagedAuditDelivered {
		t.Fatalf("record=%#v ok=%v", record, ok)
	}
}

func TestRuntimeCloseReleasesStateLockWhenCleanCloseContextExpires(t *testing.T) {
	server := newCredentialServer(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := testConfig(root, server)
	config.ProjectRoots = nil
	firstMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(firstMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with expired clean-close context = %v", err)
	}

	config.JoinToken = ""
	secondMoby := &verticalMobyEngine{}
	config.DockerOpen = func(identity dockeradapter.IdentityProvider) (Docker, error) {
		return dockeradapter.New(secondMoby, identity, dockeradapter.MinimumAPIVersion)
	}
	reopened, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatalf("state lock was not released after failed clean close: %v", err)
	}
	if !reopened.Startup().PreviousUnclean {
		t.Fatalf("restart did not preserve unclean shutdown evidence: %+v", reopened.Startup())
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Embedding only the core methods proves that a production Adapter cannot
// silently advertise a partially assembled product surface.
type verticalMobyEngineWithoutStreams struct{ verticalMobyCore }

type verticalMobyCore struct{}

func (verticalMobyCore) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	return client.PingResult{APIVersion: "1.55"}, nil
}
func (verticalMobyCore) ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
	return client.ServerVersionResult{Version: "29.0.0", APIVersion: "1.55"}, nil
}
func (verticalMobyCore) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: []container.Summary{{
		ID: verticalAgentID, Names: []string{"/dockpilot-agent"},
		Labels: map[string]string{agentsafety.AgentRoleLabel: agentsafety.AgentRoleValue},
	}}}, nil
}
func (verticalMobyCore) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, nil
}
func (verticalMobyCore) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}
func (verticalMobyCore) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, nil
}
func (verticalMobyCore) ContainerRestart(context.Context, string, client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	return client.ContainerRestartResult{}, nil
}
func (verticalMobyCore) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, nil
}
func (verticalMobyCore) Close() error { return nil }
