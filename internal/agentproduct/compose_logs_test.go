package agentproduct

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/logrelay"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/safefile"
)

const composeLogProjectUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type composeLogProjects struct{ project agentprojects.Project }

func (projects composeLogProjects) Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus) {
	return []agentprojects.Project{projects.project}, agentprojects.ScanStatus{}
}
func (projects composeLogProjects) ProjectSnapshot(uid string) (agentprojects.Project, bool) {
	if uid != projects.project.UID {
		return agentprojects.Project{}, false
	}
	return projects.project, true
}
func (projects composeLogProjects) Project(_ context.Context, uid string) (composeexec.Project, bool, error) {
	if uid != projects.project.UID {
		return composeexec.Project{}, false, nil
	}
	return composeexec.Project{
		WorkingDir: projects.project.WorkingDir, Files: append([]string(nil), projects.project.ComposeFiles...), Name: projects.project.Name,
	}, true, nil
}
func (composeLogProjects) Rescan(context.Context) error                { return nil }
func (composeLogProjects) RescanProject(context.Context, string) error { return nil }
func (composeLogProjects) FilesystemMutationAllowed(context.Context, string) (bool, string) {
	return true, ""
}
func (projects composeLogProjects) ApprovedReadOnlyFiles(_ context.Context, uid string) ([]safefile.ApprovedFile, bool, error) {
	if uid != projects.project.UID {
		return nil, false, nil
	}
	return append([]safefile.ApprovedFile(nil), projects.project.ReadOnlyFiles...), true, nil
}

type composeLogRunner struct {
	mu     sync.Mutex
	spec   composeexec.Spec
	result composeexec.Result
}

func (runner *composeLogRunner) Run(_ context.Context, spec composeexec.Spec, output chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	runner.mu.Lock()
	runner.spec = spec
	result := runner.result
	runner.mu.Unlock()
	output <- composeexec.OutputChunk{Stream: composeexec.StreamStdout, Data: []byte("web | ready\n")}
	output <- composeexec.OutputChunk{DroppedBytes: 7}
	return result, nil
}

type collectedLogSender struct{ events []producttransport.LogEvent }

func (sender *collectedLogSender) Send(event producttransport.LogEvent) error {
	sender.events = append(sender.events, event)
	return nil
}

func TestProjectComposeLogsUseBoundedTypedRelay(t *testing.T) {
	projects := composeLogProjects{project: agentprojects.Project{
		UID: composeLogProjectUID, WorkingDir: "/srv/app", ComposeFiles: []string{"/srv/app/compose.yaml"},
		Name: "app", Services: []string{"web"}, ComposeExecutable: true,
		Files: []projectmodel.FileFact{{Path: "/srv/app/compose.yaml", SHA256: strings.Repeat("a", 64)}},
	}}
	runner := &composeLogRunner{result: composeexec.Result{ExitCode: 0, RelayDroppedBytes: 7}}
	relay, err := logrelay.New(logrelay.Config{
		Source: composeLogSource{
			docker: logrelay.SourceFunc(func(context.Context, logrelay.Request, func(logrelay.Chunk) error) error {
				return errors.New("container source should not be used")
			}),
			compose: runner, projects: projects,
		},
		BytesPerSecond: 1 << 20, MaxBufferedBytes: 1 << 20, MaxBufferedChunks: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &collectedLogSender{}
	err = (producttransport.LogRelayHandler{Relay: relay}).StreamLogs(context.Background(), producttransport.SessionInfo{}, producttransport.LogRequest{
		ProjectUID: composeLogProjectUID, Services: []string{"web"}, Follow: true, TailLines: 100,
		ShowStdout: true, ShowStderr: true, Timestamps: true,
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if runner.spec.Operation != composeexec.OperationLogs || runner.spec.Project.WorkingDir != "/srv/app" ||
		len(runner.spec.Services) != 1 || runner.spec.Services[0] != "web" || !runner.spec.Flags.LogsFollow ||
		runner.spec.Flags.LogsTail != 100 || !runner.spec.Flags.LogsTimestamps || runner.spec.OutputTailBytes != 1 {
		t.Fatalf("compose log spec = %+v", runner.spec)
	}
	if len(sender.events) != 2 || string(sender.events[0].Data) != "web | ready\n" || sender.events[0].Stream != "STDOUT" ||
		sender.events[0].DroppedBytes+sender.events[1].DroppedBytes != 7 || !sender.events[1].Terminal {
		t.Fatalf("log events = %+v", sender.events)
	}
}

func TestProjectComposeLogsRejectMixedOrUnknownTargets(t *testing.T) {
	projects := composeLogProjects{project: agentprojects.Project{
		UID: composeLogProjectUID, WorkingDir: "/srv/app", ComposeFiles: []string{"/srv/app/compose.yaml"},
		Name: "app", Services: []string{"web"}, ComposeExecutable: true,
	}}
	source := composeLogSource{docker: logrelay.SourceFunc(func(context.Context, logrelay.Request, func(logrelay.Chunk) error) error { return nil }), compose: &composeLogRunner{}, projects: projects}
	if err := source.Stream(context.Background(), logrelay.Request{ProjectUID: composeLogProjectUID, ContainerID: strings.Repeat("b", 64), ShowStdout: true, ShowStderr: true}, func(logrelay.Chunk) error { return nil }); err == nil {
		t.Fatal("mixed target was accepted")
	}
	if err := source.Stream(context.Background(), logrelay.Request{ProjectUID: composeLogProjectUID, Services: []string{"unknown"}, ShowStdout: true, ShowStderr: true}, func(logrelay.Chunk) error { return nil }); err == nil {
		t.Fatal("unknown service was accepted")
	}
	if err := source.Stream(context.Background(), logrelay.Request{ProjectUID: composeLogProjectUID, Services: []string{"web"}, ShowStdout: true}, func(logrelay.Chunk) error { return nil }); err == nil {
		t.Fatal("single compose stream selection was accepted")
	}

	stream, err := logrelay.New(logrelay.Config{Source: source, BytesPerSecond: 1, MaxBufferedBytes: 1, MaxBufferedChunks: 1})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := stream.Open(context.Background(), logrelay.Request{ContainerID: "", Services: []string{"web"}})
	if err == nil {
		_ = opened.Close()
		t.Fatal("service-only generic target was accepted")
	}
}
