//go:build linux

package agentstorage

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/east-true/docklattice/internal/agentprojects"
	"github.com/east-true/docklattice/internal/safefile"
)

type ProjectSnapshot interface {
	Snapshot() ([]agentprojects.Project, agentprojects.ScanStatus)
}

// ProjectStagingReclaimer turns the verified discovery catalog into explicit
// safefile root capabilities. It never scans an arbitrary host tree.
//
// Reclaimed project bytes are deliberately not credited to the global
// state-root byte target: a project can be on another filesystem and, even on
// the same filesystem, it is outside AgentStateBytes. File-write and restore
// staging perform an FD-bound per-operation project-filesystem admission
// check; proactive capability monitoring remains scoped to the state root.
type ProjectStagingReclaimer struct {
	projects ProjectSnapshot
}

var _ FileStagingDiskPressureReclaimer = (*ProjectStagingReclaimer)(nil)

func NewProjectStagingReclaimer(projects ProjectSnapshot) (*ProjectStagingReclaimer, error) {
	if projects == nil {
		return nil, errors.New("agentstorage: project snapshot is required for staging reclaim")
	}
	return &ProjectStagingReclaimer{projects: projects}, nil
}

func (reclaimer *ProjectStagingReclaimer) ReclaimAbandonedStagingForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	projects, _ := reclaimer.projects.Snapshot()
	unique := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if project.WorkingDir != "" {
			unique[project.WorkingDir] = struct{}{}
		}
	}
	roots := make([]string, 0, len(unique))
	for root := range unique {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, path := range roots {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		root, err := safefile.OpenRoot(path, nil)
		if err != nil {
			return 0, err
		}
		// Project roots may be on arbitrary filesystems, so no one root's
		// logical byte count proves that state-root pressure was relieved.
		// Exhaust every catalog root before later, irreversible tiers.
		_, reclaimErr := root.ReclaimAbandonedStagingForDiskPressure(ctx, math.MaxInt64)
		closeErr := root.Close()
		if reclaimErr != nil || closeErr != nil {
			return 0, errors.Join(reclaimErr, closeErr)
		}
	}
	return 0, nil
}
