package agentops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/backup"
	"github.com/east-true/docklattice/internal/composeexec"
	"github.com/east-true/docklattice/internal/config"
	"github.com/east-true/docklattice/internal/diskbudget"
	"github.com/east-true/docklattice/internal/dockeradapter"
	"github.com/east-true/docklattice/internal/operation"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/safefile"
)

type fakeDocker struct {
	mu    sync.Mutex
	calls []string
}

func (d *fakeDocker) add(value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, value)
	return nil
}
func (d *fakeDocker) Start(context.Context, string) error   { return d.add("start") }
func (d *fakeDocker) Stop(context.Context, string) error    { return d.add("stop") }
func (d *fakeDocker) Restart(context.Context, string) error { return d.add("restart") }
func (d *fakeDocker) Remove(context.Context, string, dockeradapter.RemoveOptions) error {
	return d.add("remove")
}

type fakeProjects struct{}

func (fakeProjects) Project(context.Context, string) (composeexec.Project, bool, error) {
	return composeexec.Project{
		WorkingDir: "/srv/p", Files: []string{"/srv/p/compose.yml"}, Name: "p",
		Services: []composeexec.Service{{Name: "web", Image: "example/web:1", Active: true}},
	}, true, nil
}
func (fakeProjects) FilesystemMutationAllowed(context.Context, string) (bool, string) {
	return true, ""
}
func (fakeProjects) ApprovedReadOnlyFiles(context.Context, string) ([]safefile.ApprovedFile, bool, error) {
	return nil, true, nil
}

type fakeRescan struct{}

func (fakeRescan) Rescan(context.Context) error                { return nil }
func (fakeRescan) RescanProject(context.Context, string) error { return nil }

type recordingRescan struct {
	mu       sync.Mutex
	projects []string
}

func (*recordingRescan) Rescan(context.Context) error { return nil }
func (r *recordingRescan) RescanProject(_ context.Context, projectUID string) error {
	r.mu.Lock()
	r.projects = append(r.projects, projectUID)
	r.mu.Unlock()
	return nil
}

type fakeBackups struct{}

func (fakeBackups) Create(context.Context, backup.CreateRequest) (backup.Backup, error) {
	return backup.Backup{}, nil
}
func (fakeBackups) Restore(context.Context, backup.RestoreRequest) (backup.RestoreResult, error) {
	return backup.RestoreResult{}, nil
}
func (fakeBackups) CheckChangeAllowed(string) error              { return nil }
func (fakeBackups) PruneAutomatic(string, int) ([]string, error) { return nil, nil }

type fakeCompose struct {
	started chan struct{}
	fail    error
}

func (f fakeCompose) Run(ctx context.Context, _ composeexec.Spec, relay chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	if f.fail != nil {
		return composeexec.Result{ExitCode: 1}, f.fail
	}
	if f.started != nil {
		close(f.started)
	}
	if f.started != nil {
		<-ctx.Done()
		return composeexec.Result{ExitCode: -1, Canceled: true, Tail: []byte("partial")}, nil
	}
	relay <- composeexec.OutputChunk{Data: []byte("done")}
	return composeexec.Result{ExitCode: 0, Tail: []byte("done")}, nil
}

func TestExecutorFailureIsFailedNotCanceled(t *testing.T) {
	service, engine := newService(t, fakeCompose{fail: errors.New("compose failed")}, config.V1Defaults().OperationTimeout)
	_, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "op-failed", Type: "compose.up", ProjectKey: "project",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "op-failed")
	if record.Status != operation.StatusFailed || !record.CancelRequestedAt.IsZero() {
		t.Fatalf("terminal = %#v", record)
	}
}

func TestSuccessfulComposeUpRefreshesOnlyItsProject(t *testing.T) {
	engine := operation.NewDefault()
	rescan := &recordingRescan{}
	service, err := New(Config{
		Engine: engine, Docker: &fakeDocker{}, Compose: fakeCompose{}, Projects: fakeProjects{}, Approvals: fakeProjects{}, Filesystem: fakeProjects{}, Rescanner: rescan,
		Backups: fakeBackups{}, Admission: DiskAdmitterFunc(func(context.Context, diskbudget.Operation) error { return nil }), Timeouts: config.V1Defaults().OperationTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "targeted-refresh", Type: string(operation.TypeComposeUp), ProjectKey: "project",
	}); err != nil {
		t.Fatal(err)
	}
	if record := waitTerminal(t, engine, "targeted-refresh"); record.Status != operation.StatusSuccess {
		t.Fatalf("operation = %#v", record)
	}
	rescan.mu.Lock()
	defer rescan.mu.Unlock()
	if len(rescan.projects) != 1 || rescan.projects[0] != "project" {
		t.Fatalf("targeted refreshes = %#v", rescan.projects)
	}
}

func newService(t *testing.T, compose Compose, timeouts config.OperationTimeouts) (*Service, *operation.Engine) {
	t.Helper()
	engine := operation.NewDefault()
	service, err := New(Config{
		Engine: engine, Docker: &fakeDocker{}, Compose: compose, Projects: fakeProjects{}, Approvals: fakeProjects{}, Filesystem: fakeProjects{}, Rescanner: fakeRescan{},
		Backups: fakeBackups{}, Admission: DiskAdmitterFunc(func(context.Context, diskbudget.Operation) error { return nil }), Timeouts: timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, engine
}

func waitTerminal(t *testing.T, engine *operation.Engine, id string) operation.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if record, ok := engine.Get(id); ok && record.Status.Terminal() {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation %s did not become terminal", id)
	return operation.Record{}
}

func TestContainerOperationReturnsAcceptedThenSucceeds(t *testing.T) {
	service, engine := newService(t, fakeCompose{}, config.V1Defaults().OperationTimeout)
	response, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "op-container", Type: "container.start", Target: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "requested" {
		t.Fatalf("immediate response = %#v", response)
	}
	record := waitTerminal(t, engine, "op-container")
	if record.Status != operation.StatusSuccess || record.Phase != operation.PhaseFinalizing {
		t.Fatalf("terminal = %#v", record)
	}
}

func TestUserCancelReachesComposeRunnerAndKeepsPartialMarker(t *testing.T) {
	started := make(chan struct{})
	service, engine := newService(t, fakeCompose{started: started}, config.V1Defaults().OperationTimeout)
	_, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "op-compose", Type: "compose.up", ProjectKey: "project",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if outcome, err := engine.CancelWithError("op-compose", operation.CancelReasonUser); err != nil || outcome != operation.CancelAccepted {
		t.Fatalf("cancel = %s, %v", outcome, err)
	}
	record := waitTerminal(t, engine, "op-compose")
	if record.Status != operation.StatusCanceled || !record.PartialEffectsPossible || !strings.Contains(string(record.OutputTail), "partial") {
		t.Fatalf("terminal = %#v", record)
	}
}

func TestTimeoutUsesOperationCancelPath(t *testing.T) {
	started := make(chan struct{})
	timeouts := config.V1Defaults().OperationTimeout
	timeouts.ComposePull = 10 * time.Millisecond
	service, engine := newService(t, fakeCompose{started: started}, timeouts)
	_, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "op-timeout", Type: "compose.pull", ProjectKey: "project",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "op-timeout")
	if record.Status != operation.StatusCanceled || record.CancelReason != operation.CancelReasonTimeout {
		t.Fatalf("terminal = %#v", record)
	}
}

func TestComposeBuildPolicySelectsImagesAndBlocksBuildRequiredTargets(t *testing.T) {
	models := []composeexec.Service{
		{Name: "api", Image: "company/api:1.8", HasBuild: true, Active: true},
		{Name: "db", Image: "postgres:18", Active: true},
		{Name: "worker", HasBuild: true, Active: true},
		{Name: "tool", Image: "company/tool:1", HasBuild: true, PullPolicy: "build", Active: true},
		{Name: "inactive", HasBuild: true, Profiles: []string{"debug"}},
	}
	services, err := composeMutationServices(operation.TypeComposePull, "", models)
	if err != nil || strings.Join(services, ",") != "api,db" {
		t.Fatalf("project Pull services = %v, %v", services, err)
	}
	if _, err := composeMutationServices(operation.TypeComposeUp, "", models); !errors.Is(err, ErrComposeBuildRequired) ||
		!strings.Contains(err.Error(), "worker") || !strings.Contains(err.Error(), "tool") || strings.Contains(err.Error(), "inactive") {
		t.Fatalf("project Up policy error = %v", err)
	}
	if services, err := composeMutationServices(operation.TypeComposeUp, "api", models); err != nil || strings.Join(services, ",") != "api" {
		t.Fatalf("mixed Service Up = %v, %v", services, err)
	}
	for _, target := range []string{"worker", "tool", "inactive"} {
		if _, err := composeMutationServices(operation.TypeComposeUp, target, models); err == nil {
			t.Fatalf("Service Up %q unexpectedly allowed", target)
		}
	}
}

func TestComposeBuildPolicyChecksTargetDependencies(t *testing.T) {
	models := []composeexec.Service{
		{Name: "api", Image: "company/api:1.8", DependsOn: []string{"worker"}, Active: true},
		{Name: "worker", HasBuild: true, Active: true},
	}
	if _, err := composeMutationServices(operation.TypeComposeUp, "api", models); !errors.Is(err, ErrComposeBuildRequired) || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("dependency policy error = %v", err)
	}
}

func TestExactIdempotencyAndUnsupportedPayload(t *testing.T) {
	service, _ := newService(t, fakeCompose{}, config.V1Defaults().OperationTimeout)
	request := producttransport.OperationRequest{OperationID: "same", Type: "container.stop", Target: strings.Repeat("a", 64)}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err != nil {
		t.Fatal(err)
	}
	request.Target = strings.Repeat("b", 64)
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err == nil || !operation.HasErrorCode(err, operation.CodeSpecMismatch) {
		t.Fatalf("spec mismatch error = %v", err)
	}
	request = producttransport.OperationRequest{OperationID: "payload", Type: "container.start", Target: strings.Repeat("c", 64), Payload: []byte(`{"force":true}`)}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err == nil {
		t.Fatal("unsupported payload succeeded")
	}
	request = producttransport.OperationRequest{OperationID: "file", Type: "env.write", ProjectKey: "project", Target: ".env"}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err == nil || !strings.Contains(err.Error(), "JSON payload is required") {
		t.Fatalf("invalid file payload error = %v", err)
	}
}
