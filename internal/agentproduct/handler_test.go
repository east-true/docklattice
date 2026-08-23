package agentproduct

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/diskbudget"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/livestats"
	"github.com/east-true/dockpilot/internal/logrelay"
	"github.com/east-true/dockpilot/internal/operation"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/safefile"
)

var errAuditDelegated = errors.New("audit delegated")

type fakeControl struct {
	mu         sync.Mutex
	heartbeats int
	audits     int
}

func (control *fakeControl) Heartbeat(context.Context, producttransport.SessionInfo, time.Time) (producttransport.Capability, error) {
	control.mu.Lock()
	control.heartbeats++
	control.mu.Unlock()
	return producttransport.Capability{ConnectionReady: true, DockerReady: true}, nil
}

func (control *fakeControl) SyncAudit(context.Context, producttransport.SessionInfo, producttransport.AuditSyncStream) error {
	control.mu.Lock()
	control.audits++
	control.mu.Unlock()
	return errAuditDelegated
}

type fakeDocker struct {
	started chan string
}

func (docker *fakeDocker) List(context.Context) ([]dockeradapter.Container, error) {
	return []dockeradapter.Container{{ID: strings.Repeat("a", 64), Image: "example:test"}}, nil
}
func (docker *fakeDocker) Inspect(_ context.Context, id string) (dockeradapter.Container, error) {
	return dockeradapter.Container{ID: id, Image: "example:test"}, nil
}
func (docker *fakeDocker) Start(_ context.Context, id string) error { docker.started <- id; return nil }
func (docker *fakeDocker) Stop(context.Context, string) error       { return nil }
func (docker *fakeDocker) Restart(context.Context, string) error    { return nil }
func (docker *fakeDocker) Remove(context.Context, string, dockeradapter.RemoveOptions) error {
	return nil
}

type fakeProjects struct{}

func (fakeProjects) Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus) {
	return nil, agentprojects.ScanStatus{}
}
func (fakeProjects) ProjectSnapshot(string) (agentprojects.Project, bool) {
	return agentprojects.Project{}, false
}
func (fakeProjects) Project(context.Context, string) (composeexec.Project, bool, error) {
	return composeexec.Project{}, false, nil
}
func (fakeProjects) ApprovedReadOnlyFiles(context.Context, string) ([]safefile.ApprovedFile, bool, error) {
	return nil, true, nil
}
func (fakeProjects) Rescan(context.Context) error                { return nil }
func (fakeProjects) RescanProject(context.Context, string) error { return nil }
func (fakeProjects) FilesystemMutationAllowed(context.Context, string) (bool, string) {
	return true, ""
}

type fakeFiles struct{}

func (fakeFiles) Read(context.Context, string, string) (safefile.File, error) {
	return safefile.File{}, nil
}

type fakeBackups struct{}

func (fakeBackups) List(context.Context, string) ([]backup.Metadata, error) { return nil, nil }
func (fakeBackups) LoadManifest(string, string) (backup.Manifest, error)    { return backup.Manifest{}, nil }
func (fakeBackups) RecoveryBlocked(string) bool                             { return false }
func (fakeBackups) Create(context.Context, backup.CreateRequest) (backup.Backup, error) {
	return backup.Backup{}, nil
}
func (fakeBackups) Restore(context.Context, backup.RestoreRequest) (backup.RestoreResult, error) {
	return backup.RestoreResult{}, nil
}
func (fakeBackups) CheckChangeAllowed(string) error              { return nil }
func (fakeBackups) PruneAutomatic(string, int) ([]string, error) { return nil, nil }

type fakeCompose struct{}

func (fakeCompose) Run(context.Context, composeexec.Spec, chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	return composeexec.Result{ExitCode: 0}, nil
}

type logSender struct {
	mu     sync.Mutex
	events []producttransport.LogEvent
}

func (sender *logSender) Send(event producttransport.LogEvent) error {
	sender.mu.Lock()
	sender.events = append(sender.events, event)
	sender.mu.Unlock()
	return nil
}

type statsSender struct {
	cancel context.CancelFunc
	seen   chan producttransport.StatsSample
}

func (sender statsSender) Send(sample producttransport.StatsSample) error {
	select {
	case sender.seen <- sample:
	default:
	}
	sender.cancel()
	return context.Canceled
}

func validConfig(t *testing.T) (Config, *fakeControl, *fakeDocker) {
	t.Helper()
	control := &fakeControl{}
	docker := &fakeDocker{started: make(chan string, 1)}
	return Config{
		Control: control, Docker: docker, Projects: fakeProjects{}, Files: fakeFiles{}, Backups: fakeBackups{},
		Engine: operation.NewDefault(), Compose: fakeCompose{},
		Admission: agentAdmissionFunc(func(context.Context, diskbudget.Operation) error { return nil }),
		LogSource: logrelay.SourceFunc(func(_ context.Context, request logrelay.Request, emit func(logrelay.Chunk) error) error {
			return emit(logrelay.Chunk{Data: []byte("ready\n"), Stream: logrelay.Stdout, LineCount: 1})
		}),
		StatsSource: livestats.SourceFunc(func(ctx context.Context, containerID string, emit func(livestats.Sample) error) error {
			if err := emit(livestats.Sample{ContainerID: containerID, CPUPercent: 12.5}); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}),
		StatsSampleInterval: time.Millisecond,
		MatrixDocker:        &fakeMatrixDocker{},
		MatrixFrameInterval: time.Millisecond,
	}, control, docker
}

type agentAdmissionFunc func(context.Context, diskbudget.Operation) error

func (function agentAdmissionFunc) AdmitOperation(ctx context.Context, kind diskbudget.Operation) error {
	return function(ctx, kind)
}
func (agentAdmissionFunc) AdmitProjectStaging(context.Context, int64, int64, int64) error {
	return nil
}

func TestHandlerAssemblesAndDelegatesCompleteProductSurface(t *testing.T) {
	config, control, docker := validConfig(t)
	handler, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	recoveryOperation, created, err := config.Engine.Create(context.Background(), operation.Spec{
		OperationID: "recover-1", ProjectKey: "project-recovery", Type: operation.TypeComposeUp,
	})
	if err != nil || !created {
		t.Fatalf("create recovery operation = %v, %v", created, err)
	}
	lookup, err := handler.GetOperation(context.Background(), producttransport.SessionInfo{}, producttransport.GetOperationRequest{OperationID: "recover-1"})
	if err != nil || !lookup.Found || lookup.Operation.Status != string(operation.StatusRequested) || lookup.Operation.Revision == 0 {
		t.Fatalf("operation lookup = %+v, %v", lookup, err)
	}
	canceled, err := handler.CancelOperation(context.Background(), producttransport.SessionInfo{}, producttransport.CancelOperationRequest{
		OperationID: "recover-1", Reason: string(operation.CancelReasonUser),
	})
	if err != nil || canceled.Outcome != string(operation.CancelAccepted) || canceled.Operation.Revision <= lookup.Operation.Revision {
		t.Fatalf("operation cancel = %+v, %v", canceled, err)
	}
	repeated, err := handler.CancelOperation(context.Background(), producttransport.SessionInfo{}, producttransport.CancelOperationRequest{
		OperationID: "recover-1", Reason: string(operation.CancelReasonTimeout),
	})
	if err != nil || repeated.Outcome != string(operation.CancelAccepted) || repeated.Operation.Revision != canceled.Operation.Revision {
		t.Fatalf("idempotent operation cancel = %+v, %v", repeated, err)
	}
	if recoveryOperation.Snapshot().CancelReason != operation.CancelReasonUser {
		t.Fatalf("repeated cancel changed durable reason = %+v", recoveryOperation.Snapshot())
	}
	if err := recoveryOperation.TransitionStatus(operation.StatusCanceled, "", "test cancellation"); err != nil {
		t.Fatal(err)
	}
	missing, err := handler.CancelOperation(context.Background(), producttransport.SessionInfo{}, producttransport.CancelOperationRequest{
		OperationID: "missing", Reason: string(operation.CancelReasonUser),
	})
	if err != nil || missing.Outcome != string(operation.CancelNotFound) || missing.Operation.Status != "" ||
		missing.Operation.Revision != 0 || len(missing.Operation.OutputTail) != 0 {
		t.Fatalf("missing operation cancel = %+v, %v", missing, err)
	}
	if missingLookup, err := handler.GetOperation(context.Background(), producttransport.SessionInfo{}, producttransport.GetOperationRequest{OperationID: "missing"}); err != nil || missingLookup.Found {
		t.Fatalf("missing operation lookup = %+v, %v", missingLookup, err)
	}

	capability, err := handler.Heartbeat(context.Background(), producttransport.SessionInfo{}, time.Now())
	if err != nil || !capability.ConnectionReady || !capability.DockerReady {
		t.Fatalf("heartbeat = %+v, %v", capability, err)
	}
	if err := handler.SyncAudit(context.Background(), producttransport.SessionInfo{}, nil); !errors.Is(err, errAuditDelegated) {
		t.Fatalf("Audit delegation error = %v", err)
	}

	query, err := handler.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: "container.list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(query.Payload), strings.Repeat("a", 64)) || !strings.Contains(string(query.Payload), "example:test") {
		t.Fatalf("container query payload = %s", query.Payload)
	}

	containerID := strings.Repeat("b", 64)
	response, err := handler.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "op-1", Type: string(operation.TypeContainerStart), Target: containerID,
	})
	if err != nil || response.Status != string(operation.StatusRequested) {
		t.Fatalf("operation response = %+v, %v", response, err)
	}
	select {
	case started := <-docker.started:
		if started != containerID {
			t.Fatalf("started container = %q", started)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("container operation was not dispatched")
	}

	logs := &logSender{}
	if err := handler.StreamLogs(context.Background(), producttransport.SessionInfo{}, producttransport.LogRequest{ContainerID: containerID}, logs); err != nil {
		t.Fatal(err)
	}
	logs.mu.Lock()
	if len(logs.events) != 2 || string(logs.events[0].Data) != "ready\n" || !logs.events[1].Terminal {
		t.Fatalf("log events = %+v", logs.events)
	}
	logs.mu.Unlock()

	statsCtx, cancelStats := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStats()
	seen := make(chan producttransport.StatsSample, 1)
	err = handler.StreamStats(statsCtx, producttransport.SessionInfo{}, producttransport.StatsRequest{ContainerID: containerID}, statsSender{cancel: cancelStats, seen: seen})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stats stream error = %v", err)
	}
	select {
	case sample := <-seen:
		if sample.ContainerID != containerID || sample.CPUPercent != 12.5 {
			t.Fatalf("stats sample = %+v", sample)
		}
	default:
		t.Fatal("stats sample was not relayed")
	}

	control.mu.Lock()
	defer control.mu.Unlock()
	if control.heartbeats != 1 || control.audits != 1 {
		t.Fatalf("control calls: heartbeat=%d audit=%d", control.heartbeats, control.audits)
	}
}

func TestListActiveOperationsIsAuthoritativeSortedAndExcludesTerminal(t *testing.T) {
	engine := operation.NewDefault()
	zActive, created, err := engine.Create(context.Background(), operation.Spec{
		OperationID: "z-active", ProjectKey: "z-project", Target: "service-z", Type: operation.TypeDiscoveryRescan,
	})
	if err != nil || !created {
		t.Fatalf("create z-active = %v, %v", created, err)
	}
	if _, err := zActive.WriteOutput([]byte("bounded tail")); err != nil {
		t.Fatal(err)
	}
	if _, created, err := engine.Create(context.Background(), operation.Spec{
		OperationID: "a-active", ProjectKey: "a-project", Target: "service-a", Type: operation.TypeComposePull,
	}); err != nil || !created {
		t.Fatalf("create a-active = %v, %v", created, err)
	}
	terminal, created, err := engine.Create(context.Background(), operation.Spec{
		OperationID: "m-terminal", Type: operation.TypeDiscoveryRescan,
	})
	if err != nil || !created {
		t.Fatalf("create terminal = %v, %v", created, err)
	}
	if err := terminal.TransitionStatus(operation.StatusRejected, "", "test rejection"); err != nil {
		t.Fatal(err)
	}

	handler := &Handler{engine: engine}
	response, err := handler.ListActiveOperations(context.Background(), producttransport.SessionInfo{}, producttransport.ListActiveOperationsRequest{})
	if err != nil || len(response.Operations) != 2 {
		t.Fatalf("active operations = %#v, %v", response, err)
	}
	if response.Operations[0].OperationID != "a-active" || response.Operations[1].OperationID != "z-active" ||
		response.Operations[0].Type != string(operation.TypeComposePull) || response.Operations[1].Target != "service-z" ||
		string(response.Operations[1].Operation.OutputTail) != "bounded tail" {
		t.Fatalf("active operations = %#v", response.Operations)
	}
	response.Operations[1].Operation.OutputTail[0] = 'X'
	if current, ok := engine.Get("z-active"); !ok || string(current.OutputTail) != "bounded tail" {
		t.Fatalf("transport response aliases Engine record: %#v, %v", current, ok)
	}
}

func TestListActiveOperationsAfterRestartExcludesRecoveredInterrupted(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	newEngine := func() *operation.Engine {
		t.Helper()
		journal, err := operation.NewFileJournal(state, nil)
		if err != nil {
			t.Fatal(err)
		}
		config := operation.DefaultConfig()
		config.Journal = journal
		engine, err := operation.New(config)
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}

	engine := newEngine()
	running, created, err := engine.Create(context.Background(), operation.Spec{
		OperationID: "restore-running", ProjectKey: "project", Type: operation.TypeBackupRestore,
	})
	if err != nil || !created {
		t.Fatalf("create running operation = %v, %v", created, err)
	}
	if err := running.TransitionStatus(operation.StatusDispatched, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := running.TransitionStatus(operation.StatusRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := running.AdvancePhase(operation.PhaseExecuting); err != nil {
		t.Fatal(err)
	}
	if err := running.EnterCommit(); err != nil {
		t.Fatal(err)
	}
	if _, err := running.WriteOutput([]byte("last useful output")); err != nil {
		t.Fatal(err)
	}
	if err := running.FlushOutputTail(); err != nil {
		t.Fatal(err)
	}
	before := running.Snapshot()
	if active, err := (&Handler{engine: engine}).ListActiveOperations(context.Background(), producttransport.SessionInfo{}, producttransport.ListActiveOperationsRequest{}); err != nil || len(active.Operations) != 1 {
		t.Fatalf("pre-restart active operations = %#v, %v", active, err)
	}

	restarted := newEngine()
	handler := &Handler{engine: restarted}
	active, err := handler.ListActiveOperations(context.Background(), producttransport.SessionInfo{}, producttransport.ListActiveOperationsRequest{})
	if err != nil || len(active.Operations) != 0 {
		t.Fatalf("post-restart active operations = %#v, %v", active, err)
	}
	recovered, err := handler.GetOperation(context.Background(), producttransport.SessionInfo{}, producttransport.GetOperationRequest{OperationID: "restore-running"})
	if err != nil || !recovered.Found || recovered.Operation.Status != string(operation.StatusInterrupted) ||
		recovered.Operation.Phase != string(operation.PhaseCommitting) || !recovered.Operation.PartialEffectsPossible ||
		recovered.Operation.Revision != before.Revision+1 || string(recovered.Operation.OutputTail) != "last useful output" {
		t.Fatalf("recovered operation = %#v, %v", recovered, err)
	}
}

func TestHandlerRejectsPartialAssemblyAndClosesStats(t *testing.T) {
	config, _, _ := validConfig(t)
	config.Control = producttransport.AgentHandlerFunc(func(context.Context, producttransport.SessionInfo, time.Time) (producttransport.Capability, error) {
		return producttransport.Capability{}, nil
	})
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "Audit sync") {
		t.Fatalf("missing Audit error = %v", err)
	}

	config, _, _ = validConfig(t)
	config.LogSource = nil
	if _, err := New(config); err == nil {
		t.Fatal("partial product assembly succeeded")
	}

	config, _, _ = validConfig(t)
	handler, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.GetOperation(context.Background(), producttransport.SessionInfo{}, producttransport.GetOperationRequest{}); err == nil {
		t.Fatal("empty operation lookup ID succeeded")
	}
	if _, err := handler.CancelOperation(context.Background(), producttransport.SessionInfo{}, producttransport.CancelOperationRequest{
		OperationID: "op", Reason: "UNKNOWN",
	}); err == nil {
		t.Fatal("unknown cancel reason succeeded")
	}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	err = handler.StreamStats(context.Background(), producttransport.SessionInfo{}, producttransport.StatsRequest{ContainerID: strings.Repeat("c", 64)}, statsSender{})
	if !errors.Is(err, livestats.ErrClosed) {
		t.Fatalf("stats after close = %v", err)
	}
}
