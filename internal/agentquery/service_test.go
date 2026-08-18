//go:build linux

package agentquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/discovery"
	"github.com/east-true/dockpilot/internal/dockeradapter"
	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/safefile"
)

const testProjectUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testContainerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testManifestHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

type fakeDocker struct {
	mu         sync.Mutex
	containers []dockeradapter.Container
	images     []dockeradapter.Image
	networks   []dockeradapter.Network
	volumes    []dockeradapter.Volume
	inspected  string
	err        error
}

func (f *fakeDocker) List(context.Context) ([]dockeradapter.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockeradapter.Container(nil), f.containers...), f.err
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (dockeradapter.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = id
	return f.containers[0], f.err
}

func (f *fakeDocker) ListImages(context.Context) ([]dockeradapter.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockeradapter.Image(nil), f.images...), f.err
}

func (f *fakeDocker) ListNetworks(context.Context) ([]dockeradapter.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockeradapter.Network(nil), f.networks...), f.err
}

func (f *fakeDocker) ListVolumes(context.Context) ([]dockeradapter.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockeradapter.Volume(nil), f.volumes...), f.err
}

type fakeProjects struct {
	projects []agentprojects.Project
	status   agentprojects.ScanStatus
}

func (f *fakeProjects) Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus) {
	return append([]agentprojects.Project(nil), f.projects...), f.status
}

func (f *fakeProjects) ProjectSnapshot(uid string) (agentprojects.Project, bool) {
	for _, project := range f.projects {
		if project.UID == uid {
			return project, true
		}
	}
	return agentprojects.Project{}, false
}

func (f *fakeProjects) Project(_ context.Context, uid string) (composeexec.Project, bool, error) {
	project, found := f.ProjectSnapshot(uid)
	if !found || project.Stale || !project.ComposeExecutable || project.Name == "" {
		return composeexec.Project{}, false, nil
	}
	return composeexec.Project{
		WorkingDir: project.WorkingDir, Files: append([]string(nil), project.ComposeFiles...), Name: project.Name,
	}, true, nil
}

type fakeCompose struct {
	mu     sync.Mutex
	spec   composeexec.Spec
	result composeexec.Result
	err    error
}

func (f *fakeCompose) Run(_ context.Context, spec composeexec.Spec, _ chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = spec
	result := f.result
	result.Tail = append([]byte(nil), result.Tail...)
	return result, f.err
}

type fakeFiles struct {
	mu      sync.Mutex
	files   map[string]safefile.File
	project string
	path    string
}

func (f *fakeFiles) Read(_ context.Context, project, path string) (safefile.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project, f.path = project, path
	file, ok := f.files[path]
	if !ok {
		return safefile.File{}, fs.ErrNotExist
	}
	file.Content = append([]byte(nil), file.Content...)
	return file, nil
}

type fakeBackups struct {
	mu       sync.Mutex
	metadata []backup.Metadata
	project  string
	err      error
}

func (f *fakeBackups) List(_ context.Context, project string) ([]backup.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project = project
	return append([]backup.Metadata(nil), f.metadata...), f.err
}

func newTestService(t *testing.T) (*Service, *fakeDocker, *fakeProjects, *fakeFiles, *fakeBackups) {
	t.Helper()
	docker := &fakeDocker{containers: []dockeradapter.Container{{ID: testContainerID}}}
	projects := &fakeProjects{}
	files := &fakeFiles{files: map[string]safefile.File{}}
	backups := &fakeBackups{}
	compose := &fakeCompose{result: composeexec.Result{ExitCode: 0}}
	service, err := New(Config{Docker: docker, Projects: projects, Files: files, Backups: backups, Compose: compose})
	if err != nil {
		t.Fatal(err)
	}
	return service, docker, projects, files, backups
}

func query(t *testing.T, service *Service, request producttransport.QueryRequest, output any) error {
	t.Helper()
	response, err := service.Query(context.Background(), producttransport.SessionInfo{}, request)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(response.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		t.Fatalf("decode response %s: %v", response.Payload, err)
	}
	return nil
}

func TestNewRequiresEveryReadOnlyBoundary(t *testing.T) {
	_, docker, projects, files, backups := newTestService(t)
	compose := &fakeCompose{}
	configs := []Config{
		{Projects: projects, Files: files, Backups: backups, Compose: compose},
		{Docker: docker, Files: files, Backups: backups, Compose: compose},
		{Docker: docker, Projects: projects, Backups: backups, Compose: compose},
		{Docker: docker, Projects: projects, Files: files, Compose: compose},
		{Docker: docker, Projects: projects, Files: files, Backups: backups},
	}
	for _, config := range configs {
		if _, err := New(config); err == nil {
			t.Fatal("New accepted a missing dependency")
		}
	}
}

func TestContainerQueriesAreTypedValidatedAndDeterministic(t *testing.T) {
	service, docker, _, _, _ := newTestService(t)
	docker.containers = []dockeradapter.Container{
		{ID: strings.Repeat("d", 64), Names: []string{"/z", "/a"}, Mounts: []dockeradapter.Mount{{Destination: "/z"}, {Destination: "/a"}}},
		{ID: testContainerID, Image: "image", Labels: map[string]string{"b": "2", "a": "1"}},
	}
	var listed []Container
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryContainerList}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != testContainerID || listed[1].Names[0] != "/a" || listed[1].Mounts[0].Destination != "/a" {
		t.Fatalf("nondeterministic container list: %+v", listed)
	}
	var inspected Container
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryContainerInspect, Target: testContainerID}, &inspected); err != nil {
		t.Fatal(err)
	}
	if docker.inspected != testContainerID || inspected.ID == "" {
		t.Fatalf("inspect = %+v target=%q", inspected, docker.inspected)
	}
	for _, invalid := range []producttransport.QueryRequest{
		{Kind: QueryContainerList, Target: "unexpected"},
		{Kind: QueryContainerInspect, Target: "short"},
		{Kind: QueryContainerInspect, Target: testContainerID, Payload: []byte(`{}`)},
	} {
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, invalid); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid request %+v error = %v", invalid, err)
		}
	}
}

func TestHostInventoryQueriesAreTypedDeterministicAndBounded(t *testing.T) {
	service, docker, _, _, _ := newTestService(t)
	docker.images = []dockeradapter.Image{
		{ID: "sha256:" + strings.Repeat("e", 64), RepoTags: []string{"z:latest", "a:latest"}, Size: 20, Containers: -1},
		{ID: "sha256:" + strings.Repeat("d", 64), RepoDigests: []string{"demo@sha256:" + strings.Repeat("d", 64)}, Size: 10},
	}
	docker.networks = []dockeradapter.Network{
		{ID: strings.Repeat("f", 64), Name: "z", Driver: "bridge", Scope: "local"},
		{ID: strings.Repeat("a", 64), Name: "a", Driver: "bridge", Scope: "local", Internal: true},
	}
	docker.volumes = []dockeradapter.Volume{
		{Name: "z", Driver: "local", Scope: "local"},
		{Name: "a", Driver: "local", Scope: "local", CreatedAt: "2026-08-15T00:00:00Z"},
	}

	var images []Image
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryImageList}, &images); err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].ID != "sha256:"+strings.Repeat("d", 64) || images[1].RepoTags[0] != "a:latest" {
		t.Fatalf("images = %+v", images)
	}
	var networks []Network
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryNetworkList}, &networks); err != nil {
		t.Fatal(err)
	}
	if len(networks) != 2 || networks[0].Name != "a" || !networks[0].Internal {
		t.Fatalf("networks = %+v", networks)
	}
	var volumes []Volume
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryVolumeList}, &volumes); err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 || volumes[0].Name != "a" {
		t.Fatalf("volumes = %+v", volumes)
	}
	for _, kind := range []string{QueryImageList, QueryNetworkList, QueryVolumeList} {
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: kind, Target: "unexpected"}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s target error = %v", kind, err)
		}
	}

	docker.images[0].Size = -1
	if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: QueryImageList}); err == nil {
		t.Fatal("invalid image facts were accepted")
	}
}

func TestProjectQueriesUseExplicitStableJSON(t *testing.T) {
	service, _, projects, _, _ := newTestService(t)
	at := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	projects.projects = []agentprojects.Project{{
		UID: testProjectUID, Root: "/srv", WorkingDir: "/srv/app", Name: "app",
		Files:    []projectmodel.FileFact{{Path: "/srv/app/compose.yaml", Size: 10, SHA256: testManifestHash}},
		Services: []string{"web"}, IncludedWorkDirs: []string{"/srv/child"}, SourceGraphComplete: true,
		SourceReferences:   []agentprojects.SourceReference{{Kind: "include", Path: "/srv/child/compose.yaml", Accessible: true}},
		CurrentFingerprint: testManifestHash, ComposeExecutable: true,
	}}
	projects.status = agentprojects.ScanStatus{ScannedAt: at, Truncated: true, StopReason: discovery.StopMaxDirectories, DirectoriesSeen: 4, LastScannedPath: "/srv/z"}
	var listed ProjectListResponse
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryProjectList}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Projects) != 1 || listed.Projects[0].UID != testProjectUID || listed.Status.StopReason != string(discovery.StopMaxDirectories) ||
		!listed.Projects[0].SourceGraphComplete || fmt.Sprint(listed.Projects[0].IncludedWorkDirs) != "[/srv/child]" ||
		len(listed.Projects[0].SourceReferences) != 1 || listed.Projects[0].SourceReferences[0].Path != "/srv/child/compose.yaml" {
		t.Fatalf("projects = %+v", listed)
	}
	var status ScanStatus
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryProjectStatus}, &status); err != nil {
		t.Fatal(err)
	}
	if status.DirectoriesSeen != 4 || !status.ScannedAt.Equal(at) {
		t.Fatalf("status = %+v", status)
	}
}

func TestProjectSnapshotQueryReturnsOnlyRequestedProject(t *testing.T) {
	service, _, projects, _, _ := newTestService(t)
	projects.projects = []agentprojects.Project{
		{UID: testProjectUID, Root: "/srv", WorkingDir: "/srv/app", Name: "app", ComposeExecutable: true,
			Files:              []projectmodel.FileFact{{Path: "/srv/app/compose.yaml", Size: 10, SHA256: testManifestHash}},
			CurrentFingerprint: testManifestHash},
		{UID: strings.Repeat("d", 64), Root: "/srv", WorkingDir: "/srv/other", Name: "other", ComposeExecutable: true,
			Files:              []projectmodel.FileFact{{Path: "/srv/other/compose.yaml", Size: 10, SHA256: strings.Repeat("e", 64)}},
			CurrentFingerprint: strings.Repeat("e", 64)},
	}
	var response ProjectSnapshotResponse
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryProjectSnapshot, Target: testProjectUID}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Project.UID != testProjectUID || response.Project.WorkingDir != "/srv/app" || len(response.Project.Files) != 1 {
		t.Fatalf("targeted project = %+v", response.Project)
	}
	for _, request := range []producttransport.QueryRequest{
		{Kind: QueryProjectSnapshot},
		{Kind: QueryProjectSnapshot, Target: "not-a-project"},
		{Kind: QueryProjectSnapshot, Target: testProjectUID, Payload: []byte(`{}`)},
	} {
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid targeted snapshot %+v error=%v", request, err)
		}
	}
	if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: QueryProjectSnapshot, Target: strings.Repeat("f", 64)}); !errors.Is(err, ErrProjectUnavailable) {
		t.Fatalf("missing targeted snapshot error=%v", err)
	}
}

func TestProjectListReportsRawComposeDockerFacts(t *testing.T) {
	service, docker, _, _, _ := newTestService(t)
	docker.containers = []dockeradapter.Container{
		{ID: testContainerID, Labels: map[string]string{
			"com.docker.compose.project":              "sample",
			"com.docker.compose.project.working_dir":  "/srv/sample",
			"com.docker.compose.project.config_files": "/srv/sample/compose.yaml,/srv/sample/compose.prod.yaml",
			"com.docker.compose.service":              "web",
			"com.docker.compose.config-hash":          "opaque-compose-internal-value",
		}},
		{ID: strings.Repeat("d", 64), Labels: map[string]string{"unrelated": "container"}},
	}
	var listed ProjectListResponse
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryProjectList}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.DockerFacts) != 1 {
		t.Fatalf("Docker facts = %+v", listed.DockerFacts)
	}
	fact := listed.DockerFacts[0]
	if fact.ContainerID != testContainerID || fact.ProjectName != "sample" || fact.WorkingDir != "/srv/sample" ||
		fact.Service != "web" || fact.ConfigHash != "opaque-compose-internal-value" ||
		len(fact.ConfigFiles) != 2 || fact.ConfigFiles[1] != "/srv/sample/compose.prod.yaml" {
		t.Fatalf("Docker fact = %+v", fact)
	}
}

func TestComposeQueriesAreProjectScopedStrictAndBounded(t *testing.T) {
	service, docker, projects, files, backups := newTestService(t)
	projects.projects = []agentprojects.Project{{
		UID: testProjectUID, WorkingDir: "/srv/app", ComposeFiles: []string{"/srv/app/compose.yaml"},
		Name: "app", ComposeExecutable: true,
	}}
	compose := &fakeCompose{result: composeexec.Result{ExitCode: 0, Tail: []byte("NAME  STATE\napp   running\n")}}
	var err error
	service, err = New(Config{Docker: docker, Projects: projects, Files: files, Backups: backups, Compose: compose})
	if err != nil {
		t.Fatal(err)
	}

	var output ComposeOutput
	if err := query(t, service, producttransport.QueryRequest{
		Kind: QueryComposePS, Target: testProjectUID, Payload: []byte(`{"services":["web"],"all":true}`),
	}, &output); err != nil {
		t.Fatal(err)
	}
	if output.Output != "NAME  STATE\napp   running\n" || compose.spec.Operation != composeexec.OperationPS ||
		!compose.spec.Flags.PSAll || len(compose.spec.Services) != 1 || compose.spec.Services[0] != "web" ||
		compose.spec.OutputTailBytes != maxComposeQueryBytes || compose.spec.Project.WorkingDir != "/srv/app" {
		t.Fatalf("ps spec=%+v output=%+v", compose.spec, output)
	}

	compose.result = composeexec.Result{ExitCode: 0, Tail: []byte("services:\n  web:\n")}
	if err := query(t, service, producttransport.QueryRequest{
		Kind: QueryComposeConfig, Target: testProjectUID, Payload: []byte(`{"services":["web"]}`),
	}, &output); err != nil {
		t.Fatal(err)
	}
	if compose.spec.Operation != composeexec.OperationConfig || !compose.spec.Flags.ConfigNoInterpolate || output.Output == "" {
		t.Fatalf("config spec=%+v output=%+v", compose.spec, output)
	}

	for _, request := range []producttransport.QueryRequest{
		{Kind: QueryComposePS, Target: testProjectUID},
		{Kind: QueryComposePS, Target: testProjectUID, Payload: []byte(`{"services":["web","web"],"all":false}`)},
		{Kind: QueryComposePS, Target: testProjectUID, Payload: []byte(`{"services":["--help"],"all":false}`)},
		{Kind: QueryComposePS, Target: strings.Repeat("b", 64), Payload: []byte(`{"services":[],"all":false}`)},
	} {
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, request); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrProjectUnavailable) {
			t.Fatalf("request %+v error = %v", request, err)
		}
	}
	compose.result = composeexec.Result{ExitCode: 0, TailTruncated: true, Tail: make([]byte, maxComposeQueryBytes)}
	if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{
		Kind: QueryComposePS, Target: testProjectUID, Payload: []byte(`{"services":[],"all":false}`),
	}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("truncated Compose output error = %v", err)
	}
}

func TestFileReadUsesProjectRelativeTypedPayloadAndMarksEnvSecret(t *testing.T) {
	service, _, _, files, _ := newTestService(t)
	at := time.Date(2026, 8, 15, 2, 3, 4, 0, time.UTC)
	files.files[".env.local"] = safefile.File{
		RelativePath: ".env.local", Content: []byte("TOKEN=secret\n"), SHA256: testManifestHash,
		MTime: at, Mode: 0o640, LineEndings: safefile.LineEndingsLF,
	}
	var result FileResponse
	request := producttransport.QueryRequest{Kind: QueryFileRead, Target: testProjectUID, Payload: []byte(`{"relative_path":".env.local"}`)}
	if err := query(t, service, request, &result); err != nil {
		t.Fatal(err)
	}
	if files.project != testProjectUID || files.path != ".env.local" || !result.Secret || result.Content != "TOKEN=secret\n" || result.Mode != 0o640 {
		t.Fatalf("file response = %+v read=%s/%s", result, files.project, files.path)
	}
	for _, payload := range []string{
		``, `{"relative_path":".env","extra":true}`, `{"relative_path":".env"} trailing`, `{"relative_path":""}`,
	} {
		request.Payload = []byte(payload)
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("payload %q error = %v", payload, err)
		}
	}
}

func TestProjectEnvironmentTreatsEveryValueAsSecret(t *testing.T) {
	service, _, _, files, _ := newTestService(t)
	files.files[".env"] = safefile.File{RelativePath: ".env", Content: []byte("# ignored\nZ=plain # note\nexport A='quoted secret'\nB=\"line\\nvalue\"\n")}
	var result []EnvironmentEntry
	request := producttransport.QueryRequest{Kind: QueryProjectEnvironment, Target: testProjectUID}
	if err := query(t, service, request, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 || result[0].Name != "A" || result[0].Value != "quoted secret" || result[1].Value != "line\nvalue" {
		t.Fatalf("environment = %+v", result)
	}
	for _, entry := range result {
		if !entry.Secret {
			t.Fatalf("environment value was not marked secret: %+v", entry)
		}
	}
	files.files[".env"] = safefile.File{RelativePath: ".env", Content: []byte("A=first\nA=second\n")}
	if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, request); err == nil || strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "second") {
		t.Fatalf("duplicate secret error = %v", err)
	}
}

func TestBackupListReturnsOnlyMetadataInNewestFirstOrder(t *testing.T) {
	service, _, _, _, backups := newTestService(t)
	older := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	backups.metadata = []backup.Metadata{
		{BackupID: "20260814T010000.000000000Z-0123456789abcdef", ProjectUID: testProjectUID, CreatedAt: older, Trigger: backup.TriggerManual, FileCount: 2, SizeBytes: 50, ManifestSHA256: testManifestHash},
		{BackupID: "20260814T020000.000000000Z-fedcba9876543210", ProjectUID: testProjectUID, CreatedAt: newer, Trigger: backup.TriggerPreWrite, FileCount: 1, SizeBytes: 25, ManifestSHA256: testManifestHash},
	}
	var result []BackupMetadata
	if err := query(t, service, producttransport.QueryRequest{Kind: QueryBackupList, Target: testProjectUID}, &result); err != nil {
		t.Fatal(err)
	}
	if backups.project != testProjectUID || len(result) != 2 || !result[0].CreatedAt.Equal(newer) {
		t.Fatalf("backups = %+v project=%q", result, backups.project)
	}
	payload, _ := json.Marshal(result)
	for _, forbidden := range []string{"working_dir", "storage_path", "files.tar", "manifest.json"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("backup metadata leaked %q: %s", forbidden, payload)
		}
	}
}

func TestRejectsUnknownOversizedAndOversizedResponse(t *testing.T) {
	service, docker, _, _, _ := newTestService(t)
	requests := []producttransport.QueryRequest{
		{Kind: "unknown"},
		{Kind: strings.Repeat("k", 65)},
		{Kind: QueryFileRead, Target: testProjectUID, Payload: make([]byte, maxRequestPayloadBytes+1)},
	}
	if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, requests[0]); !errors.Is(err, ErrUnsupportedQuery) {
		t.Fatalf("unknown error = %v", err)
	}
	for _, request := range requests[1:] {
		if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid envelope error = %v", err)
		}
	}
	docker.containers = []dockeradapter.Container{{ID: testContainerID, Labels: map[string]string{"huge": strings.Repeat("x", producttransport.DefaultMaxMessageBytes)}}}
	response, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: QueryContainerList})
	if !errors.Is(err, ErrResponseTooLarge) || response.Payload != nil {
		t.Fatalf("oversized response = %d, %v", len(response.Payload), err)
	}
}

func TestSafeFilesUsesCatalogRootAndSafefilePolicy(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "not-allowed.txt"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	projects := &fakeProjects{projects: []agentprojects.Project{{UID: testProjectUID, WorkingDir: directory}}}
	reader, err := NewSafeFiles(projects)
	if err != nil {
		t.Fatal(err)
	}
	file, err := reader.Read(context.Background(), testProjectUID, ".env")
	if err != nil || string(file.Content) != "A=1\n" {
		t.Fatalf("safe read = %+v, %v", file, err)
	}
	if _, err := reader.Read(context.Background(), testProjectUID, "not-allowed.txt"); !errors.Is(err, safefile.ErrPath) {
		t.Fatalf("allowlist rejection = %v", err)
	}
	if _, err := reader.Read(context.Background(), strings.Repeat("f", 64), ".env"); !errors.Is(err, ErrProjectUnavailable) {
		t.Fatalf("unknown project = %v", err)
	}
	if err := os.Mkdir(filepath.Join(directory, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config", "service.env"), []byte("B=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projects.projects[0].ReadOnlyFiles = []safefile.ApprovedFile{{RelativePath: "config/service.env", Access: safefile.ReadOnly}}
	file, err = reader.Read(context.Background(), testProjectUID, "config/service.env")
	if err != nil || string(file.Content) != "B=2\n" {
		t.Fatalf("approved env file = %+v, %v", file, err)
	}
}

func TestConcurrentQueriesAreRaceSafe(t *testing.T) {
	service, _, _, files, _ := newTestService(t)
	files.files[".env"] = safefile.File{RelativePath: ".env", Content: []byte("A=1\n")}
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := service.Query(context.Background(), producttransport.SessionInfo{}, producttransport.QueryRequest{Kind: QueryProjectEnvironment, Target: testProjectUID}); err != nil {
				t.Errorf("query: %v", err)
			}
		}()
	}
	wait.Wait()
}
