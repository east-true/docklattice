package agentproduct

import (
	"context"
	"errors"

	"github.com/east-true/dockpilot/internal/agentprojects"
	"github.com/east-true/dockpilot/internal/safefile"
)

var ErrProjectFileUnavailable = errors.New("agentproduct: project file is unavailable")

type ProjectSnapshot interface {
	Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus)
}

// ProjectFiles resolves the Server-visible stable project UID to an
// Agent-owned verified working directory for every read. It intentionally
// does not cache file descriptors across discovery rescans.
type ProjectFiles struct{ projects ProjectSnapshot }

func NewProjectFiles(projects ProjectSnapshot) (*ProjectFiles, error) {
	if projects == nil {
		return nil, errors.New("agentproduct: project catalog is required")
	}
	return &ProjectFiles{projects: projects}, nil
}

func (files *ProjectFiles) Read(ctx context.Context, projectUID, relativePath string) (safefile.File, error) {
	projects, _ := files.projects.Snapshot()
	for _, project := range projects {
		if project.UID != projectUID {
			continue
		}
		root, err := safefile.OpenRoot(project.WorkingDir, project.ReadOnlyFiles)
		if err != nil {
			return safefile.File{}, err
		}
		defer root.Close()
		return root.Read(ctx, relativePath)
	}
	return safefile.File{}, ErrProjectFileUnavailable
}
