//go:build linux

package composesource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAnalyzeExtractsBoundedSourceProvenanceAndSafeApprovals(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	for _, directory := range []string{project, filepath.Join(project, "parts"), filepath.Join(project, "shared"), filepath.Join(root, "sibling")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSource(t, filepath.Join(project, "compose.yaml"), `
include:
  - path: parts/child.yaml
  - ../sibling/compose.yaml
services:
  app:
    extends:
      file: shared/base.yaml
      service: base
`)
	writeSource(t, filepath.Join(project, "parts", "child.yaml"), "include: nested.yaml\nservices: {}\n")
	writeSource(t, filepath.Join(project, "parts", "nested.yaml"), "services: {}\n")
	writeSource(t, filepath.Join(project, "shared", "base.yaml"), "services: {}\n")
	writeSource(t, filepath.Join(root, "sibling", "compose.yaml"), "services: {}\n")

	result, err := New().Analyze(context.Background(), root, project, []string{filepath.Join(project, "compose.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete {
		t.Fatalf("result unexpectedly incomplete: %#v", result)
	}
	if got := referenceSummary(result.References); got !=
		"extends:base.yaml:true:true,include:child.yaml:true:true,include:nested.yaml:true:true,include:compose.yaml:true:false" {
		t.Fatalf("references = %s", got)
	}
	if got := baseNames(result.Files); got != "compose.yaml,child.yaml,nested.yaml,base.yaml" {
		t.Fatalf("safe source files = %s", got)
	}
	if got := joinBaseNames(result.IncludedWorkDirs); got != "parts,sibling" {
		t.Fatalf("include directories = %s", got)
	}
	if got := joinStrings(result.ReadOnlyPaths); got != "parts/child.yaml,parts/nested.yaml,shared/base.yaml" {
		t.Fatalf("read-only approvals = %s", got)
	}
}

func TestAnalyzeFailsClosedForAliasCycleAndLimits(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(project, "compose.yaml")
	writeSource(t, main, "include: &source child.yaml\nservices: {}\n")
	result, err := New().Analyze(context.Background(), root, project, []string{main})
	if err != nil || result.Complete || len(result.References) != 0 {
		t.Fatalf("alias result = %#v err=%v", result, err)
	}

	writeSource(t, main, "include: child.yaml\nservices: {}\n")
	writeSource(t, filepath.Join(project, "child.yaml"), "include: compose.yaml\nservices: {}\n")
	result, err = New().Analyze(context.Background(), root, project, []string{main})
	if err != nil || result.Complete {
		t.Fatalf("cycle result = %#v err=%v", result, err)
	}
	if got := referenceSummary(result.References); got != "include:child.yaml:true:true,include:compose.yaml:true:true" {
		t.Fatalf("cycle references = %s", got)
	}

	limited := Analyzer{MaxFiles: 1, MaxEdges: 1, MaxDepth: 1}
	result, err = limited.Analyze(context.Background(), root, project, []string{main})
	if err != nil || result.Complete || len(result.ReadOnlyPaths) != 0 {
		t.Fatalf("limited result = %#v err=%v", result, err)
	}
}

func TestAnalyzeRejectsSymlinkAndUnclearSourcePath(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(project, "compose.yaml")
	writeSource(t, main, "include: linked.yaml\nservices: {}\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(project, "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	result, err := New().Analyze(context.Background(), root, project, []string{main})
	got := referenceSummary(result.References)
	if err != nil || result.Complete || got != "include:linked.yaml:false:false" {
		t.Fatalf("symlink result = %#v err=%v", result, err)
	}

	writeSource(t, main, "include: ${COMPOSE_FILE}\nservices: {}\n")
	result, err = New().Analyze(context.Background(), root, project, []string{main})
	if err != nil || result.Complete || len(result.References) != 0 {
		t.Fatalf("interpolated result = %#v err=%v", result, err)
	}
}

func writeSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func referenceSummary(references []Reference) string {
	values := make([]string, 0, len(references))
	for _, reference := range references {
		values = append(values, string(reference.Kind)+":"+filepath.Base(reference.Path)+":"+boolText(reference.Accessible)+":"+boolText(reference.ReadOnly))
	}
	return joinStrings(values)
}

func baseNames(files []File) string {
	values := make([]string, 0, len(files))
	for _, file := range files {
		values = append(values, filepath.Base(file.Path))
	}
	return joinStrings(values)
}

func joinBaseNames(paths []string) string {
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		values = append(values, filepath.Base(path))
	}
	return joinStrings(values)
}

func joinStrings(values []string) string {
	result := ""
	for index, value := range values {
		if index != 0 {
			result += ","
		}
		result += value
	}
	return result
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
