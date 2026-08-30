//go:build linux

package agentquery

import (
	"context"
	"fmt"

	"github.com/east-true/docklattice/internal/agentprojects"
	"github.com/east-true/docklattice/internal/safefile"
)

// SafeFiles resolves only a catalog-owned project UID, then delegates all
// path authorization and TOCTOU-safe reads to safefile.Root.
type SafeFiles struct{ projects ProjectCatalog }

func NewSafeFiles(projects ProjectCatalog) (*SafeFiles, error) {
	if projects == nil {
		return nil, fmt.Errorf("agentquery: project catalog is required for file reads")
	}
	return &SafeFiles{projects: projects}, nil
}

func (reader *SafeFiles) Read(ctx context.Context, projectUID, relativePath string) (safefile.File, error) {
	projects, _ := reader.projects.Snapshot()
	var project *agentprojects.Project
	for index := range projects {
		if projects[index].UID == projectUID {
			project = &projects[index]
			break
		}
	}
	if project == nil {
		return safefile.File{}, ErrProjectUnavailable
	}
	root, err := safefile.OpenRoot(project.WorkingDir, project.ReadOnlyFiles)
	if err != nil {
		return safefile.File{}, err
	}
	defer root.Close()
	return root.Read(ctx, relativePath)
}
