package agentproduct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/east-true/docklattice/internal/agentprojects"
	"github.com/east-true/docklattice/internal/safefile"
)

type snapshotProjects []agentprojects.Project

func (projects snapshotProjects) Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus) {
	return append([]agentprojects.Project(nil), projects...), agentprojects.ScanStatus{}
}

func TestProjectFilesResolvesUIDAndUsesSafeFileBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewProjectFiles(snapshotProjects{{UID: "project-1", WorkingDir: root}})
	if err != nil {
		t.Fatal(err)
	}
	read, err := files.Read(context.Background(), "project-1", "compose.yaml")
	if err != nil || string(read.Content) != "services: {}\n" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if _, err := files.Read(context.Background(), "missing", "compose.yaml"); !errors.Is(err, ErrProjectFileUnavailable) {
		t.Fatalf("missing project error = %v", err)
	}
	if _, err := files.Read(context.Background(), "project-1", "../secret"); err == nil {
		t.Fatal("traversal was accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "service.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err = NewProjectFiles(snapshotProjects{{
		UID: "project-1", WorkingDir: root,
		ReadOnlyFiles: []safefile.ApprovedFile{{RelativePath: "config/service.env", Access: safefile.ReadOnly}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	read, err = files.Read(context.Background(), "project-1", "config/service.env")
	if err != nil || string(read.Content) != "A=1\n" {
		t.Fatalf("approved env file = %+v, %v", read, err)
	}
}
