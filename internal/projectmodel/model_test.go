package projectmodel

import (
	"errors"
	"testing"
)

const testAgentID = "dcfc202f-9e6c-4d1c-889d-00321933534a"

func fact(path, hashByte string) FileFact {
	return FileFact{Path: path, Size: 10, SHA256: repeat(hashByte, 64)}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func TestUIDStableAcrossNameAndRejectsNonCanonicalInput(t *testing.T) {
	first, err := UID(testAgentID, "/srv/stacks/app")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := UID(testAgentID, "/srv/stacks/app")
	if first != second || len(first) != 64 {
		t.Fatalf("UIDs = %q %q", first, second)
	}
	if _, err := UID(testAgentID, "/srv/stacks/app/"); !errors.Is(err, ErrInvalidFact) {
		t.Fatalf("trailing-slash error = %v", err)
	}
}

func TestFingerprintAndChangedFilesIgnoreOrderMtimeAndSize(t *testing.T) {
	left := []FileFact{fact("/srv/app/.env", "a"), fact("/srv/app/compose.yaml", "b")}
	right := []FileFact{fact("/srv/app/compose.yaml", "b"), fact("/srv/app/.env", "a")}
	right[0].Size = 999
	first, err := Fingerprint(left)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Fingerprint(right)
	if first != second {
		t.Fatalf("fingerprints differ: %s %s", first, second)
	}
	changed, err := ChangedFiles(left, right)
	if err != nil || len(changed) != 0 {
		t.Fatalf("ChangedFiles = %v, %v", changed, err)
	}
	right[0].SHA256 = repeat("c", 64)
	changed, _ = ChangedFiles(left, right)
	if len(changed) != 1 || changed[0] != "/srv/app/compose.yaml" {
		t.Fatalf("ChangedFiles = %v", changed)
	}
}

func TestMergeJoinsDockerFactsIncludesAndComputesDrift(t *testing.T) {
	parentFiles := []FileFact{fact("/srv/parent/compose.yaml", "a")}
	parentUID, _ := UID(testAgentID, "/srv/parent")
	parentFingerprint, _ := Fingerprint(parentFiles)
	projects, err := Merge(testAgentID, []FilesystemProject{
		{WorkingDir: "/srv/parent", Name: "parent", Files: parentFiles, Services: []string{"api"}, IncludedWorkDirs: []string{"/srv/parent/child"}},
		{WorkingDir: "/srv/parent/child", Name: "child", Files: []FileFact{fact("/srv/parent/child/compose.yaml", "b")}},
	}, []DockerFact{
		{ContainerID: "c2", ProjectName: "parent", WorkingDir: "/srv/parent", Service: "worker", ConfigHash: "ignored"},
		{ContainerID: "c1", ProjectName: "parent-old-name", WorkingDir: "/srv/parent", Service: "api"},
	}, map[string]string{parentUID: parentFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects = %+v", projects)
	}
	parent, child := projects[0], projects[1]
	if parent.WorkingDir != "/srv/parent" || parent.Name != "parent" || !parent.Managed || parent.Drift != DriftInSync {
		t.Fatalf("parent = %+v", parent)
	}
	if len(parent.ContainerIDs) != 2 || len(parent.Services) != 2 {
		t.Fatalf("runtime merge = %+v", parent)
	}
	if len(child.IncludedBy) != 1 || child.IncludedBy[0] != parent.UID || child.Drift != DriftNoBaseline {
		t.Fatalf("child = %+v", child)
	}
}

func TestMergeMarksNameCollisionAndUnmanagedProjectsReadOnly(t *testing.T) {
	projects, err := Merge(testAgentID, []FilesystemProject{
		{WorkingDir: "/srv/a", Name: "same", Files: []FileFact{fact("/srv/a/compose.yaml", "a")}},
		{WorkingDir: "/srv/b", Name: "same", Files: []FileFact{fact("/srv/b/compose.yaml", "b")}},
	}, []DockerFact{
		{ContainerID: "outside", ProjectName: "outside", WorkingDir: "/opt/outside", Service: "web"},
		{ContainerID: "missing", ProjectName: "missing", Service: "web"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 4 {
		t.Fatalf("projects = %+v", projects)
	}
	collisions, unmanaged, missingUID := 0, 0, 0
	for _, project := range projects {
		if project.NameCollision && project.MutationBlockReason == "PROJECT_NAME_COLLISION" {
			collisions++
		}
		if !project.Managed && project.MutationBlockReason == "UNMANAGED_COMPOSE_PROJECT" {
			unmanaged++
			if project.UID == "" {
				missingUID++
			}
		}
	}
	if collisions != 2 || unmanaged != 2 || missingUID != 1 {
		t.Fatalf("collisions=%d unmanaged=%d missingUID=%d: %+v", collisions, unmanaged, missingUID, projects)
	}
}

func TestMergeRejectsDuplicateDirectoryAndInvalidHashes(t *testing.T) {
	_, err := Merge(testAgentID, []FilesystemProject{
		{WorkingDir: "/srv/a", Name: "a", Files: []FileFact{fact("/srv/a/compose.yaml", "a")}},
		{WorkingDir: "/srv/a", Name: "again", Files: []FileFact{fact("/srv/a/compose.yml", "b")}},
	}, nil, nil)
	if !errors.Is(err, ErrInvalidFact) {
		t.Fatalf("duplicate error = %v", err)
	}
	_, err = Fingerprint([]FileFact{{Path: "/srv/a/compose.yaml", SHA256: "not-a-hash"}})
	if !errors.Is(err, ErrInvalidFact) {
		t.Fatalf("hash error = %v", err)
	}
}
