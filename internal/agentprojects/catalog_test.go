package agentprojects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeconfig"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/composesource"
	"github.com/east-true/dockpilot/internal/discovery"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/safefile"
)

type fakeScanner struct {
	results []discovery.Result
	errors  []error
	call    int
}

type fakeTargetedScanner struct {
	*fakeScanner
	targetFiles []discovery.File
	targetErr   error
	targetCalls int
}

func (s *fakeTargetedScanner) ScanProject(context.Context, string, string) ([]discovery.File, error) {
	s.targetCalls++
	return append([]discovery.File(nil), s.targetFiles...), s.targetErr
}

func (s *fakeScanner) Scan(context.Context) (discovery.Result, error) {
	index := s.call
	s.call++
	return s.results[index], s.errors[index]
}

type fakeEvaluator struct {
	calls  int
	inputs [][]string
}

func (e *fakeEvaluator) Evaluate(_ context.Context, dir string, files []string) (composeconfig.Result, error) {
	e.calls++
	e.inputs = append(e.inputs, append([]string(nil), files...))
	return composeconfig.Result{Project: composeexec.Project{WorkingDir: dir, Files: files, Name: "demo"}, Services: []string{"web"}}, nil
}

type sequenceEvaluator struct {
	calls  int
	errors []error
}

func (e *sequenceEvaluator) Evaluate(_ context.Context, dir string, files []string) (composeconfig.Result, error) {
	index := e.calls
	e.calls++
	if index < len(e.errors) && e.errors[index] != nil {
		return composeconfig.Result{}, e.errors[index]
	}
	return composeconfig.Result{Project: composeexec.Project{WorkingDir: dir, Files: files, Name: "demo"}, Services: []string{"web"}}, nil
}

type redirectingEvaluator struct{}

func (redirectingEvaluator) Evaluate(context.Context, string, []string) (composeconfig.Result, error) {
	return composeconfig.Result{Project: composeexec.Project{
		WorkingDir: "/outside", Files: []string{"/outside/compose.yml"}, Name: "redirected",
	}}, nil
}

type envFileEvaluator struct {
	calls    int
	envFiles []string
}

func (e *envFileEvaluator) Evaluate(_ context.Context, dir string, files []string) (composeconfig.Result, error) {
	e.calls++
	return composeconfig.Result{
		Project:  composeexec.Project{WorkingDir: dir, Files: append([]string(nil), files...), Name: "demo"},
		Services: []string{"web"}, EnvFiles: append([]string(nil), e.envFiles...),
	}, nil
}

func file(root, path, hash string) discovery.File {
	return discovery.File{Root: root, Path: path, Size: 10, ModTime: time.Unix(1, 0), SHA256: hash}
}

func TestRescanCachesComposeEvaluationByFingerprint(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}},
	}, errors: []error{nil, nil}}
	evaluator := &fakeEvaluator{}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 1 {
		t.Fatalf("evaluator calls = %d", evaluator.calls)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || projects[0].Name != "demo" || projects[0].Stale {
		t.Fatalf("projects = %#v", projects)
	}
	if _, ok, err := catalog.Project(context.Background(), projects[0].UID); err != nil || !ok {
		t.Fatalf("Project = %v, %v", ok, err)
	}
}

func TestRescanPassesDefaultOverrideAfterBaseFile(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		{Root: "/srv", Path: "/srv/p/compose.override.yml", Kind: discovery.FileKindCompose, Size: 10, SHA256: strings.Repeat("b", 64)},
		{Root: "/srv", Path: "/srv/p/compose.yaml", Kind: discovery.FileKindCompose, Size: 10, SHA256: strings.Repeat("a", 64)},
	}}}, errors: []error{nil}}
	evaluator := &fakeEvaluator{}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "READY" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/srv/p/compose.yaml", "/srv/p/compose.override.yml"}
	if len(evaluator.inputs) != 1 || fmt.Sprint(evaluator.inputs[0]) != fmt.Sprint(want) {
		t.Fatalf("evaluator inputs = %v, want %v", evaluator.inputs, want)
	}
	project, _ := catalog.Snapshot()
	if len(project) != 1 || fmt.Sprint(project[0].ComposeFiles) != fmt.Sprint(want) {
		t.Fatalf("Compose files = %v", project)
	}
}

func TestSourceGraphAddsSafeFilesApprovalsAndIncludeRelations(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(parent, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCatalogSource(t, filepath.Join(parent, "compose.yaml"), `
include: ../child/compose.yaml
services:
  app:
    extends:
      file: shared/base.yaml
      service: app
`)
	writeCatalogSource(t, filepath.Join(parent, "shared", "base.yaml"), "services: {}\n")
	writeCatalogSource(t, filepath.Join(child, "compose.yaml"), "services: {}\n")
	parentFact := file(root, filepath.Join(parent, "compose.yaml"), strings.Repeat("a", 64))
	childFact := file(root, filepath.Join(child, "compose.yaml"), strings.Repeat("b", 64))
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{parentFact, childFact}},
		{Files: []discovery.File{parentFact, childFact}},
		{Files: []discovery.File{parentFact, childFact}},
	}, errors: []error{nil, nil, nil}}
	agentID, _ := agentid.New()
	evaluator := &fakeEvaluator{}
	catalog, err := NewWithSourceGraph(agentID, scanner, evaluator, composesource.New(), func(string) (bool, string) { return true, "READY" }, func(string) (bool, string) { return true, "READY" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 2 || evaluator.calls != 2 {
		t.Fatalf("projects=%#v evaluator calls=%d", projects, evaluator.calls)
	}
	parentProject := projectByDir(t, projects, parent)
	got := fmtSourceReferences(parentProject.SourceReferences)
	if !parentProject.SourceGraphComplete || got != "extends:base.yaml:true:true,include:compose.yaml:true:false" {
		t.Fatalf("source graph = %#v summary=%s", parentProject.SourceReferences, got)
	}
	if got := fmt.Sprint(parentProject.IncludedWorkDirs); got != fmt.Sprint([]string{child}) {
		t.Fatalf("include dirs = %s", got)
	}
	if got := fmt.Sprint(parentProject.ReadOnlyFiles); got != "[{shared/base.yaml 1}]" {
		t.Fatalf("safe approvals = %s", got)
	}
	if got := fmtProjectFileBases(parentProject.Files); got != "compose.yaml,compose.yaml,base.yaml" {
		t.Fatalf("fingerprinted source files = %s", got)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 2 {
		t.Fatalf("unchanged complete graph was not cached: calls=%d", evaluator.calls)
	}
	writeCatalogSource(t, filepath.Join(parent, "shared", "base.yaml"), "services:\n  base: {}\n")
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 3 {
		t.Fatalf("changed source did not invalidate Docker evaluation cache: calls=%d", evaluator.calls)
	}
}

func TestIncompleteSourceGraphNeverReusesComposeEvaluation(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(projectDir, "compose.yaml")
	writeCatalogSource(t, compose, "include: &unsupported child.yaml\nservices: {}\n")
	fact := file(root, compose, strings.Repeat("a", 64))
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{fact}}, {Files: []discovery.File{fact}}}, errors: []error{nil, nil}}
	agentID, _ := agentid.New()
	evaluator := &fakeEvaluator{}
	catalog, err := NewWithSourceGraph(agentID, scanner, evaluator, composesource.New(), func(string) (bool, string) { return true, "READY" }, func(string) (bool, string) { return true, "READY" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || projects[0].SourceGraphComplete || evaluator.calls != 2 {
		t.Fatalf("projects=%#v evaluator calls=%d", projects, evaluator.calls)
	}
}

func writeCatalogSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func projectByDir(t *testing.T, projects []Project, directory string) Project {
	t.Helper()
	for _, project := range projects {
		if project.WorkingDir == directory {
			return project
		}
	}
	t.Fatalf("project %q missing from %#v", directory, projects)
	return Project{}
}

func fmtSourceReferences(references []SourceReference) string {
	result := make([]string, 0, len(references))
	for _, reference := range references {
		result = append(result, reference.Kind+":"+filepath.Base(reference.Path)+":"+fmt.Sprint(reference.Accessible)+":"+fmt.Sprint(reference.ReadOnly))
	}
	return strings.Join(result, ",")
}

func fmtProjectFileBases(files []projectmodel.FileFact) string {
	result := make([]string, 0, len(files))
	for _, file := range files {
		result = append(result, filepath.Base(file.Path))
	}
	return strings.Join(result, ",")
}

func TestRescanTracksDotEnvWithoutPassingItAsComposeFile(t *testing.T) {
	agentID, _ := agentid.New()
	composeHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	envHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	changedEnvHash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{
			{Root: "/srv", Path: "/srv/p/.env", Kind: discovery.FileKindEnv, Size: 10, ModTime: time.Unix(1, 0), SHA256: envHash},
			{Root: "/srv", Path: "/srv/p/compose.yml", Kind: discovery.FileKindCompose, Size: 10, ModTime: time.Unix(1, 0), SHA256: composeHash},
		}},
		{Files: []discovery.File{
			{Root: "/srv", Path: "/srv/p/.env", Kind: discovery.FileKindEnv, Size: 10, ModTime: time.Unix(1, 0), SHA256: changedEnvHash},
			{Root: "/srv", Path: "/srv/p/compose.yml", Kind: discovery.FileKindCompose, Size: 10, ModTime: time.Unix(1, 0), SHA256: composeHash},
		}},
	}, errors: []error{nil, nil}}
	evaluator := &fakeEvaluator{}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 2 {
		t.Fatalf("evaluator calls = %d", evaluator.calls)
	}
	wantComposeFiles := []string{"/srv/p/compose.yml"}
	for _, input := range evaluator.inputs {
		if len(input) != len(wantComposeFiles) || input[0] != wantComposeFiles[0] {
			t.Fatalf("evaluator input = %v, want %v", input, wantComposeFiles)
		}
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || len(projects[0].Files) != 2 || len(projects[0].ComposeFiles) != 1 {
		t.Fatalf("projects = %#v", projects)
	}
	project, ok, err := catalog.Project(context.Background(), projects[0].UID)
	if err != nil || !ok || len(project.Files) != 1 || project.Files[0] != "/srv/p/compose.yml" {
		t.Fatalf("Project = %#v, %v, %v", project, ok, err)
	}
}

func TestRescanProjectRefreshesOnlyRequestedCatalogEntry(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	base := &fakeScanner{results: []discovery.Result{{
		Files: []discovery.File{file("/srv", "/srv/p/compose.yml", initialHash)},
	}}, errors: []error{nil}}
	scanner := &fakeTargetedScanner{fakeScanner: base, targetFiles: []discovery.File{
		file("/srv", "/srv/p/compose.yml", updatedHash),
	}}
	evaluator := &fakeEvaluator{}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 {
		t.Fatalf("initial projects = %#v", projects)
	}
	if err := catalog.RescanProject(context.Background(), projects[0].UID); err != nil {
		t.Fatal(err)
	}
	if scanner.call != 1 || scanner.targetCalls != 1 {
		t.Fatalf("scan calls global=%d targeted=%d", scanner.call, scanner.targetCalls)
	}
	updated, found := catalog.ProjectSnapshot(projects[0].UID)
	if !found || updated.CurrentFingerprint == projects[0].CurrentFingerprint || evaluator.calls != 2 {
		t.Fatalf("targeted project = %#v evaluator calls=%d", updated, evaluator.calls)
	}
}

func TestPeriodicRescanReportsExternalKeyFileChangesWithoutFileDetails(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", initialHash)}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", updatedHash)}},
	}, errors: []error{nil, nil}}
	catalog, err := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reported []ExternalConfigChange
	if err := catalog.RescanForExternalChanges(context.Background(), func(_ context.Context, changes []ExternalConfigChange) error {
		reported = append([]ExternalConfigChange(nil), changes...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || len(reported) != 1 || reported[0].ProjectUID != projects[0].UID ||
		reported[0].ChangedFileCount != 1 || reported[0].ObservedAt.IsZero() {
		t.Fatalf("projects=%#v reported=%#v", projects, reported)
	}
}

func TestPeriodicRescanRetriesAuditBeforeAdvancingComparisonPoint(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", initialHash)}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", updatedHash)}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", updatedHash)}},
	}, errors: []error{nil, nil, nil}}
	catalog, err := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := catalog.Snapshot()
	if err := catalog.RescanForExternalChanges(context.Background(), func(context.Context, []ExternalConfigChange) error {
		return errors.New("audit WAL unavailable")
	}); err == nil {
		t.Fatal("periodic scan succeeded despite failed Audit append")
	}
	afterFailure, _ := catalog.Snapshot()
	if len(before) != 1 || len(afterFailure) != 1 || before[0].CurrentFingerprint != afterFailure[0].CurrentFingerprint {
		t.Fatalf("failed observation advanced catalog: before=%#v after=%#v", before, afterFailure)
	}
	var calls int
	if err := catalog.RescanForExternalChanges(context.Background(), func(_ context.Context, changes []ExternalConfigChange) error {
		calls++
		if len(changes) != 1 || changes[0].ChangedFileCount != 1 {
			t.Fatalf("retry changes=%#v", changes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("successful observer calls=%d", calls)
	}
}

func TestPeriodicRescanAuditsChangedFilesEvenWhenComposeEvaluationFails(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", initialHash)}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", updatedHash)}},
	}, errors: []error{nil, nil}}
	evaluator := &sequenceEvaluator{errors: []error{nil, errors.New("invalid Compose configuration")}}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := catalog.Snapshot()
	var reported []ExternalConfigChange
	err = catalog.RescanForExternalChanges(context.Background(), func(_ context.Context, changes []ExternalConfigChange) error {
		reported = append([]ExternalConfigChange(nil), changes...)
		return nil
	})
	if err == nil || !IsPublishedDegraded(err) || !strings.Contains(err.Error(), "invalid Compose configuration") {
		t.Fatalf("periodic scan error=%v", err)
	}
	projects, _ := catalog.Snapshot()
	if len(reported) != 1 || reported[0].ChangedFileCount != 1 || len(projects) != 1 || len(before) != 1 ||
		projects[0].CurrentFingerprint == before[0].CurrentFingerprint || projects[0].ComposeExecutable ||
		projects[0].CapabilityReason != "Compose configuration evaluation failed" {
		t.Fatalf("reported=%#v projects=%#v", reported, projects)
	}
	if _, ok, projectErr := catalog.Project(context.Background(), projects[0].UID); projectErr != nil || ok {
		t.Fatalf("failed evaluation project executable=%v error=%v", ok, projectErr)
	}
}

func TestPeriodicTruncatedRescanReportsSeenChangesButNotUnseenRemoval(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{
			file("/srv", "/srv/a/compose.yml", initialHash),
			file("/srv", "/srv/b/compose.yml", initialHash),
		}},
		{Files: []discovery.File{
			file("/srv", "/srv/a/compose.yml", updatedHash),
		}, Truncated: true, StopReason: discovery.StopMaxDirectories},
	}, errors: []error{nil, nil}}
	catalog, err := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reported []ExternalConfigChange
	if err := catalog.RescanForExternalChanges(context.Background(), func(_ context.Context, changes []ExternalConfigChange) error {
		reported = append([]ExternalConfigChange(nil), changes...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(reported) != 1 || reported[0].ChangedFileCount != 1 || len(projects) != 2 {
		t.Fatalf("reported=%#v projects=%#v", reported, projects)
	}
	for _, project := range projects {
		if project.WorkingDir == "/srv/b" && !project.Stale {
			t.Fatalf("unseen truncated project was not kept stale: %#v", project)
		}
	}
}

func TestTargetedManagedRefreshDoesNotBecomeExternalChange(t *testing.T) {
	agentID, _ := agentid.New()
	initialHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updatedHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	base := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", initialHash)}},
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", updatedHash)}},
	}, errors: []error{nil, nil}}
	scanner := &fakeTargetedScanner{fakeScanner: base, targetFiles: []discovery.File{
		file("/srv", "/srv/p/compose.yml", updatedHash),
	}}
	catalog, err := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if err := catalog.RescanProject(context.Background(), projects[0].UID); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RescanForExternalChanges(context.Background(), func(_ context.Context, changes []ExternalConfigChange) error {
		t.Fatalf("managed targeted refresh became external change: %#v", changes)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTruncatedScanPreservesUnseenAsNonExecutableStale(t *testing.T) {
	agentID, _ := agentid.New()
	hashA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/a/compose.yml", hashA), file("/srv", "/srv/b/compose.yml", hashB)}},
		{Files: []discovery.File{file("/srv", "/srv/a/compose.yml", hashA)}, Truncated: true, StopReason: discovery.StopMaxDirectories},
	}, errors: []error{nil, nil}}
	catalog, _ := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, status := catalog.Snapshot()
	if len(projects) != 2 || !status.Truncated {
		t.Fatalf("projects=%#v status=%#v", projects, status)
	}
	var stale Project
	for _, project := range projects {
		if project.WorkingDir == "/srv/b" {
			stale = project
		}
	}
	if !stale.Stale || stale.ComposeExecutable {
		t.Fatalf("stale project = %#v", stale)
	}
	if _, ok, _ := catalog.Project(context.Background(), stale.UID); ok {
		t.Fatal("stale project remained executable")
	}
}

func TestTruncatedFilesystemErrorPublishesVerifiedProjects(t *testing.T) {
	agentID, _ := agentid.New()
	scanErr := &discovery.ScanError{Code: discovery.CodeFilesystem, Path: "/srv/blocked", Err: os.ErrPermission}
	scanner := &fakeScanner{
		results: []discovery.Result{{
			Files:     []discovery.File{file("/srv", "/srv/verified/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
			Truncated: true, StopReason: discovery.StopPermissionDenied, DirectoriesSeen: 2, LastScannedPath: "/srv/blocked",
		}},
		errors: []error{scanErr},
	}
	catalog, _ := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	err := catalog.Rescan(context.Background())
	if err == nil || !IsPublishedDegraded(err) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("truncated filesystem error=%v", err)
	}
	projects, status := catalog.Snapshot()
	if len(projects) != 1 || projects[0].WorkingDir != "/srv/verified" || !projects[0].ComposeExecutable ||
		!status.Truncated || status.StopReason != discovery.StopPermissionDenied || status.LastScannedPath != "/srv/blocked" {
		t.Fatalf("projects=%#v status=%#v", projects, status)
	}
}

func TestCanceledTruncatedScanDoesNotPublish(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{
		results: []discovery.Result{{
			Files:     []discovery.File{file("/srv", "/srv/partial/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
			Truncated: true, StopReason: discovery.StopContextCanceled, DirectoriesSeen: 1, LastScannedPath: "/srv/partial",
		}},
		errors: []error{context.Canceled},
	}
	catalog, _ := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	err := catalog.Rescan(context.Background())
	if !errors.Is(err, context.Canceled) || IsPublishedDegraded(err) {
		t.Fatalf("canceled scan error=%v", err)
	}
	projects, status := catalog.Snapshot()
	if len(projects) != 0 || !status.ScannedAt.IsZero() {
		t.Fatalf("canceled scan published projects=%#v status=%#v", projects, status)
	}
}

func TestSafetyDegradedRootIsVisibleButNeverEvaluated(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		file("/srv", "/srv/p/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}}, errors: []error{nil}}
	evaluator := &fakeEvaluator{}
	catalog, _ := New(agentID, scanner, evaluator, func(string) (bool, string) { return false, "PATH_IDENTITY_MISMATCH" })
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || projects[0].ComposeExecutable || projects[0].CapabilityReason != "PATH_IDENTITY_MISMATCH" || evaluator.calls != 0 {
		t.Fatalf("projects=%#v calls=%d", projects, evaluator.calls)
	}
}

func TestFailedCleanScanDoesNotReplacePreviousCatalog(t *testing.T) {
	agentID, _ := agentid.New()
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{file("/srv", "/srv/p/compose.yml", hash)}}, {},
	}, errors: []error{nil, errors.New("filesystem failed")}}
	catalog, _ := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := catalog.Snapshot()
	if err := catalog.Rescan(context.Background()); err == nil {
		t.Fatal("failed scan succeeded")
	} else if IsPublishedDegraded(err) {
		t.Fatalf("clean failed scan was marked as published degradation: %v", err)
	}
	after, _ := catalog.Snapshot()
	if len(before) != 1 || len(after) != 1 || before[0].UID != after[0].UID {
		t.Fatalf("before=%#v after=%#v", before, after)
	}
}

func TestEvaluatorCannotRedirectVerifiedPaths(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		file("/srv", "/srv/p/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}}, errors: []error{nil}}
	catalog, _ := New(agentID, scanner, redirectingEvaluator{}, func(string) (bool, string) { return true, "" })
	if err := catalog.Rescan(context.Background()); err == nil || !strings.Contains(err.Error(), "changed verified project identity") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestRescanApprovesOnlyRootContainedRegularEnvFiles(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(projectDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config", "service.env"), []byte("TOP_SECRET=must-not-escape\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "shared.env"), []byte("OUTSIDE_PROJECT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(projectDir, "config", "linked.env")); err != nil {
		t.Fatal(err)
	}
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		file(root, filepath.Join(projectDir, "compose.yaml"), strings.Repeat("a", 64)),
	}}}, errors: []error{nil}}
	evaluator := &envFileEvaluator{envFiles: []string{
		"config/service.env", "../shared.env", "../../outside.env", filepath.Join(projectDir, "config", "linked.env"),
	}}
	catalog, err := New(agentID, scanner, evaluator, func(root string) (bool, string) { return root != "", "READY" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || evaluator.calls != 1 {
		t.Fatalf("projects=%#v evaluator calls=%d", projects, evaluator.calls)
	}
	project := projects[0]
	if got := fmtEnvFileReferences(project.EnvFiles); got != "service.env=true,shared.env=false,outside.env=false,linked.env=false" {
		t.Fatalf("env metadata = %s", got)
	}
	if len(project.ReadOnlyFiles) != 1 || project.ReadOnlyFiles[0] != (safefile.ApprovedFile{RelativePath: "config/service.env", Access: safefile.ReadOnly}) {
		t.Fatalf("approved files = %#v", project.ReadOnlyFiles)
	}
	if strings.Contains(fmt.Sprintf("%#v", project), "must-not-escape") {
		t.Fatalf("env contents escaped catalog metadata: %#v", project)
	}
	approved, found, err := catalog.ApprovedReadOnlyFiles(context.Background(), project.UID)
	if err != nil || !found || len(approved) != 1 {
		t.Fatalf("approved snapshot = %#v found=%v err=%v", approved, found, err)
	}
	approved[0].RelativePath = "mutated"
	again, _, err := catalog.ApprovedReadOnlyFiles(context.Background(), project.UID)
	if err != nil || again[0].RelativePath != "config/service.env" {
		t.Fatalf("approval copy was mutable: %#v err=%v", again, err)
	}
}

func TestServiceEnvFileContentInvalidatesEvaluationCacheAndFingerprint(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(projectDir, "compose.yaml")
	envPath := filepath.Join(projectDir, "service.env")
	writeCatalogSource(t, composePath, "services:\n  web:\n    env_file: service.env\n")
	writeCatalogSource(t, envPath, "VALUE=one\n")
	composeFact := file(root, composePath, strings.Repeat("a", 64))
	scanner := &fakeScanner{results: []discovery.Result{
		{Files: []discovery.File{composeFact}},
		{Files: []discovery.File{composeFact}},
		{Files: []discovery.File{composeFact}},
	}, errors: []error{nil, nil, nil}}
	evaluator := &envFileEvaluator{envFiles: []string{"service.env"}}
	agentID, _ := agentid.New()
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return true, "READY" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, _ := catalog.Snapshot()
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 1 {
		t.Fatalf("unchanged env_file invalidated cache: calls=%d", evaluator.calls)
	}
	writeCatalogSource(t, envPath, "VALUE=two\n")
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, _ := catalog.Snapshot()
	if evaluator.calls != 2 || first[0].CurrentFingerprint == third[0].CurrentFingerprint {
		t.Fatalf("env_file change was missed: calls=%d first=%s third=%s", evaluator.calls, first[0].CurrentFingerprint, third[0].CurrentFingerprint)
	}
	if got := fmtProjectFileBases(third[0].Files); got != "compose.yaml,service.env" {
		t.Fatalf("fingerprinted inputs = %s", got)
	}
}

func TestSafetyDegradedMountNeverEvaluatesOrApprovesEnvFiles(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		file(root, filepath.Join(projectDir, "compose.yaml"), strings.Repeat("a", 64)),
	}}}, errors: []error{nil}}
	evaluator := &envFileEvaluator{envFiles: []string{".env"}}
	catalog, err := New(agentID, scanner, evaluator, func(string) (bool, string) { return false, "PATH_IDENTITY_MISMATCH" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 || evaluator.calls != 0 || len(projects[0].ReadOnlyFiles) != 0 || len(projects[0].EnvFiles) != 0 {
		t.Fatalf("safety-degraded project = %#v evaluator calls=%d", projects, evaluator.calls)
	}
}

func fmtEnvFileReferences(references []EnvFileReference) string {
	values := make([]string, 0, len(references))
	for _, reference := range references {
		values = append(values, filepath.Base(reference.Path)+"="+fmt.Sprint(reference.Readable))
	}
	return strings.Join(values, ",")
}

func TestResolveBackupProjectUsesExactInitialCatalogIdentity(t *testing.T) {
	agentID, _ := agentid.New()
	scanner := &fakeScanner{results: []discovery.Result{{Files: []discovery.File{
		file("/srv", "/srv/p/compose.yml", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}}, errors: []error{nil}}
	catalog, err := New(agentID, scanner, &fakeEvaluator{}, func(string) (bool, string) { return true, "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := catalog.Snapshot()
	if len(projects) != 1 {
		t.Fatalf("projects = %#v", projects)
	}
	resolved, err := catalog.ResolveBackupProject(context.Background(), projects[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != (backup.Project{UID: projects[0].UID, Name: "demo", WorkingDir: "/srv/p"}) {
		t.Fatalf("resolved = %#v", resolved)
	}
	if _, err := catalog.ResolveBackupProject(context.Background(), "missing"); !errors.Is(err, backup.ErrRecoveryRequired) {
		t.Fatalf("missing resolver error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.ResolveBackupProject(canceled, projects[0].UID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolver error = %v", err)
	}
}
