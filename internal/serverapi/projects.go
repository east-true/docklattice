package serverapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/webui"
)

const (
	QueryProjectList       = "project.list"
	maxProjectSnapshots    = 4096
	maxProjectDockerFacts  = 4096
	maxProjectFiles        = 1024
	maxProjectServices     = 4096
	maxProjectSourceRefs   = 512
	maxProjectMetadataText = 4096
	projectScanTimeFormat  = "2006-01-02T15:04:05.000000000Z"
)

type agentProjectList struct {
	Projects    []agentProjectSnapshot   `json:"projects"`
	DockerFacts []agentDockerProjectFact `json:"docker_facts"`
	Status      agentProjectScanStatus   `json:"status"`
}

// agentDockerProjectFact contains public Compose label values exactly as the
// Agent observed them. Server validation bounds the transport data, but never
// recomputes or compares the internal Compose config-hash label.
type agentDockerProjectFact struct {
	ContainerID string   `json:"container_id"`
	ProjectName string   `json:"project_name"`
	WorkingDir  string   `json:"working_dir,omitempty"`
	ConfigFiles []string `json:"config_files,omitempty"`
	Service     string   `json:"service,omitempty"`
	ConfigHash  string   `json:"config_hash,omitempty"`
}

// agentProjectSnapshotResponse is the bounded post-operation observation for
// one project. It deliberately carries no whole-host scan status: a targeted
// refresh must not claim that unrelated projects were revalidated.
type agentProjectSnapshotResponse struct {
	Project agentProjectSnapshot `json:"project"`
}

type agentProjectSnapshot struct {
	UID                     string                 `json:"project_uid"`
	Root                    string                 `json:"root"`
	WorkingDir              string                 `json:"working_dir"`
	Files                   []agentProjectFileFact `json:"files"`
	Name                    string                 `json:"name"`
	Services                []string               `json:"services"`
	IncludedWorkDirs        []string               `json:"included_work_dirs,omitempty"`
	SourceReferences        []agentSourceReference `json:"source_references,omitempty"`
	SourceGraphComplete     bool                   `json:"source_graph_complete"`
	CurrentFingerprint      string                 `json:"current_fingerprint"`
	ComposeExecutable       bool                   `json:"compose_executable"`
	FilesystemWritable      bool                   `json:"filesystem_writable"`
	RestoreRecoveryRequired bool                   `json:"restore_recovery_required,omitempty"`
	CapabilityReason        string                 `json:"capability_reason,omitempty"`
	Stale                   bool                   `json:"stale"`
}

type agentSourceReference struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	ReadOnly   bool   `json:"read_only"`
}

type agentProjectFileFact struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type agentProjectScanStatus struct {
	ScannedAt       time.Time `json:"scanned_at"`
	Truncated       bool      `json:"truncated"`
	StopReason      string    `json:"stop_reason,omitempty"`
	DirectoriesSeen int       `json:"directories_seen"`
	LastScannedPath string    `json:"last_scanned_path,omitempty"`
}

type storedAppliedFingerprint struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type validatedProjectSnapshot struct {
	agentProjectSnapshot
	verifiedAt time.Time
}

func (b *Backend) syncAgentProjects(ctx context.Context, agentID string, session producttransport.ControlSession) error {
	observedAt := time.Now().UTC()
	response, err := session.Query(ctx, producttransport.QueryRequest{Kind: QueryProjectList})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: "project discovery", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return &corruptDataError{boundary: "Agent project discovery response", cause: errors.New("payload exceeds transport limit")}
	}
	var snapshot agentProjectList
	if err := decodeStrictJSON(response.Payload, &snapshot); err != nil {
		return &corruptDataError{boundary: "Agent project discovery response", cause: err}
	}
	validated, err := validateProjectSnapshot(agentID, snapshot)
	if err != nil {
		return err
	}
	dockerFacts, err := validateDockerProjectFacts(snapshot.DockerFacts)
	if err != nil {
		return err
	}
	return b.mergeProjectSnapshotWithDockerObserved(ctx, agentID, snapshot.Status, validated, dockerFacts, observedAt)
}

func (b *Backend) syncProjectAfterManagedChange(ctx context.Context, agentID, projectUID string, establishBaseline bool) error {
	if !canonicalSHA256.MatchString(projectUID) {
		return fmt.Errorf("%w: invalid project identity", webui.ErrInvalidRequest)
	}
	session, err := b.activeSession(agentID)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	response, err := session.Query(ctx, producttransport.QueryRequest{Kind: "project.snapshot", Target: projectUID})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: "targeted project discovery", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return &corruptDataError{boundary: "Agent targeted project response", cause: errors.New("payload exceeds transport limit")}
	}
	var responseBody agentProjectSnapshotResponse
	if err := decodeStrictJSON(response.Payload, &responseBody); err != nil {
		return &corruptDataError{boundary: "Agent targeted project response", cause: err}
	}
	validated, err := validateTargetedProjectSnapshot(agentID, responseBody.Project)
	if err != nil {
		return err
	}
	if validated.UID != projectUID {
		return &corruptDataError{boundary: "Agent targeted project identity", cause: errors.New("response project_uid does not match request")}
	}
	if establishBaseline && (validated.Stale || !validated.ComposeExecutable) {
		return fmt.Errorf("%w: successful compose.up did not yield a current executable project snapshot", webui.ErrConflict)
	}
	return b.mergeTargetedProjectSnapshotObserved(ctx, agentID, validated, establishBaseline, observedAt)
}

func validateTargetedProjectSnapshot(agentID string, project agentProjectSnapshot) (validatedProjectSnapshot, error) {
	validated, err := validateProjectSnapshot(agentID, agentProjectList{
		Projects: []agentProjectSnapshot{project},
		Status:   agentProjectScanStatus{ScannedAt: time.Unix(1, 0).UTC(), DirectoriesSeen: 1},
	})
	if err != nil {
		return validatedProjectSnapshot{}, err
	}
	return validated[0], nil
}

func validateProjectSnapshot(agentID string, snapshot agentProjectList) ([]validatedProjectSnapshot, error) {
	status := snapshot.Status
	if status.ScannedAt.IsZero() || status.DirectoriesSeen < 0 || len(status.StopReason) > 64 ||
		len(status.LastScannedPath) > maxProjectMetadataText || !utf8.ValidString(status.StopReason) ||
		!utf8.ValidString(status.LastScannedPath) || status.Truncated != (status.StopReason != "") ||
		status.StopReason != "" && !validProjectScanStopReason(status.StopReason) {
		return nil, &corruptDataError{boundary: "Agent project scan status", cause: errors.New("invalid scan status")}
	}
	if len(snapshot.Projects) > maxProjectSnapshots {
		return nil, &corruptDataError{boundary: "Agent project discovery response", cause: errors.New("too many projects")}
	}
	seenUID := make(map[string]struct{}, len(snapshot.Projects))
	seenDir := make(map[string]struct{}, len(snapshot.Projects))
	result := make([]validatedProjectSnapshot, len(snapshot.Projects))
	for index, item := range snapshot.Projects {
		if len(item.Root) > maxProjectMetadataText || len(item.WorkingDir) > maxProjectMetadataText ||
			len(item.Name) > 256 || len(item.CapabilityReason) > maxProjectMetadataText ||
			!utf8.ValidString(item.Root) || !utf8.ValidString(item.WorkingDir) || !utf8.ValidString(item.Name) ||
			!utf8.ValidString(item.CapabilityReason) || len(item.Files) == 0 || len(item.Files) > maxProjectFiles ||
			len(item.Services) > maxProjectServices || len(item.IncludedWorkDirs) > maxProjectSourceRefs ||
			len(item.SourceReferences) > maxProjectSourceRefs || item.ComposeExecutable && item.Name == "" ||
			item.Stale && item.ComposeExecutable || !canonicalSHA256.MatchString(item.CurrentFingerprint) ||
			!canonicalAbsolutePath(item.Root) || !canonicalAbsolutePath(item.WorkingDir) || !pathWithin(item.Root, item.WorkingDir) {
			return nil, &corruptDataError{boundary: "Agent project discovery response", cause: fmt.Errorf("invalid project %d", index)}
		}
		uid, err := projectmodel.UID(agentID, item.WorkingDir)
		if err != nil || uid != item.UID {
			return nil, &corruptDataError{boundary: "Agent project identity", cause: errors.New("project UID does not match Agent and working directory")}
		}
		if _, duplicate := seenUID[item.UID]; duplicate {
			return nil, &corruptDataError{boundary: "Agent project discovery response", cause: errors.New("duplicate project UID")}
		}
		if _, duplicate := seenDir[item.WorkingDir]; duplicate {
			return nil, &corruptDataError{boundary: "Agent project discovery response", cause: errors.New("duplicate working directory")}
		}
		seenUID[item.UID] = struct{}{}
		seenDir[item.WorkingDir] = struct{}{}

		facts := make([]projectmodel.FileFact, len(item.Files))
		for fileIndex, file := range item.Files {
			if !canonicalAbsolutePath(file.Path) || !pathWithin(item.WorkingDir, file.Path) || file.Size < 0 ||
				!canonicalSHA256.MatchString(file.SHA256) || fileIndex > 0 && item.Files[fileIndex-1].Path >= file.Path {
				return nil, &corruptDataError{boundary: "Agent project file facts", cause: errors.New("invalid or unsorted file fact")}
			}
			facts[fileIndex] = projectmodel.FileFact{Path: file.Path, Size: file.Size, SHA256: file.SHA256}
		}
		fingerprint, err := projectmodel.Fingerprint(facts)
		if err != nil || fingerprint != item.CurrentFingerprint {
			return nil, &corruptDataError{boundary: "Agent project fingerprint", cause: errors.New("fingerprint does not match file facts")}
		}
		for serviceIndex, service := range item.Services {
			if service == "" || len(service) > 256 || !utf8.ValidString(service) ||
				serviceIndex > 0 && item.Services[serviceIndex-1] >= service {
				return nil, &corruptDataError{boundary: "Agent project services", cause: errors.New("invalid or unsorted service")}
			}
		}
		for includeIndex, directory := range item.IncludedWorkDirs {
			if !canonicalAbsolutePath(directory) || !pathWithin(item.Root, directory) ||
				includeIndex > 0 && item.IncludedWorkDirs[includeIndex-1] >= directory {
				return nil, &corruptDataError{boundary: "Agent project include relations", cause: errors.New("invalid or unsorted included working directory")}
			}
		}
		for referenceIndex, reference := range item.SourceReferences {
			if (reference.Kind != "include" && reference.Kind != "extends") || !canonicalAbsolutePath(reference.Path) ||
				!pathWithin(item.Root, reference.Path) || !reference.Accessible && reference.ReadOnly ||
				reference.ReadOnly && !pathWithin(item.WorkingDir, reference.Path) ||
				referenceIndex > 0 && (item.SourceReferences[referenceIndex-1].Kind > reference.Kind ||
					item.SourceReferences[referenceIndex-1].Kind == reference.Kind && item.SourceReferences[referenceIndex-1].Path >= reference.Path) {
				return nil, &corruptDataError{boundary: "Agent project source references", cause: errors.New("invalid or unsorted source reference")}
			}
		}
		result[index] = validatedProjectSnapshot{agentProjectSnapshot: item}
		if !item.Stale {
			result[index].verifiedAt = status.ScannedAt.UTC()
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result, nil
}

func validateDockerProjectFacts(facts []agentDockerProjectFact) ([]projectmodel.DockerFact, error) {
	if len(facts) > maxProjectDockerFacts {
		return nil, &corruptDataError{boundary: "Agent Docker project facts", cause: errors.New("too many Docker facts")}
	}
	seen := make(map[string]struct{}, len(facts))
	result := make([]projectmodel.DockerFact, len(facts))
	for index, fact := range facts {
		if !canonicalContainerID.MatchString(fact.ContainerID) || fact.ProjectName == "" || len(fact.ProjectName) > 256 ||
			len(fact.WorkingDir) > maxProjectMetadataText || len(fact.Service) > 256 || len(fact.ConfigHash) > maxProjectMetadataText ||
			len(fact.ConfigFiles) > maxProjectFiles || !utf8.ValidString(fact.ProjectName) || !utf8.ValidString(fact.WorkingDir) ||
			!utf8.ValidString(fact.Service) || !utf8.ValidString(fact.ConfigHash) || strings.ContainsRune(fact.ProjectName, 0) ||
			strings.ContainsRune(fact.WorkingDir, 0) || strings.ContainsRune(fact.Service, 0) || strings.ContainsRune(fact.ConfigHash, 0) ||
			fact.WorkingDir != "" && !canonicalAbsolutePath(fact.WorkingDir) {
			return nil, &corruptDataError{boundary: "Agent Docker project facts", cause: fmt.Errorf("invalid Docker fact %d", index)}
		}
		if _, duplicate := seen[fact.ContainerID]; duplicate {
			return nil, &corruptDataError{boundary: "Agent Docker project facts", cause: errors.New("duplicate container identity")}
		}
		seen[fact.ContainerID] = struct{}{}
		configFiles := make([]string, len(fact.ConfigFiles))
		for fileIndex, path := range fact.ConfigFiles {
			if path == "" || len(path) > maxProjectMetadataText || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
				return nil, &corruptDataError{boundary: "Agent Docker project facts", cause: fmt.Errorf("invalid Docker config file %d", fileIndex)}
			}
			configFiles[fileIndex] = path
		}
		result[index] = projectmodel.DockerFact{
			ContainerID: fact.ContainerID, ProjectName: fact.ProjectName, WorkingDir: fact.WorkingDir,
			ConfigFiles: configFiles, Service: fact.Service, ConfigHash: fact.ConfigHash,
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

type mergedProjectSnapshot struct {
	snapshot        validatedProjectSnapshot
	managed         bool
	unmanagedReason string
	containerIDs    []string
	services        []string
	includedBy      []string
	collision       bool
}

// mergeProjectFacts is the Server-side identity boundary from architecture
// section 7. Agent-provided filesystem snapshots retain their capability and
// freshness state; raw Compose labels add only container/service attachment.
func mergeProjectFacts(agentID string, filesystem []validatedProjectSnapshot, dockerFacts []projectmodel.DockerFact) ([]mergedProjectSnapshot, error) {
	if len(filesystem) == 0 && len(dockerFacts) == 0 {
		return nil, nil
	}
	fsFacts := make([]projectmodel.FilesystemProject, len(filesystem))
	byUID := make(map[string]validatedProjectSnapshot, len(filesystem))
	for index, item := range filesystem {
		files := make([]projectmodel.FileFact, len(item.Files))
		for fileIndex, file := range item.Files {
			files[fileIndex] = projectmodel.FileFact{Path: file.Path, Size: file.Size, SHA256: file.SHA256}
		}
		fsFacts[index] = projectmodel.FilesystemProject{
			WorkingDir: item.WorkingDir, Name: item.Name, Files: files, Services: append([]string(nil), item.Services...),
			IncludedWorkDirs: append([]string(nil), item.IncludedWorkDirs...),
		}
		byUID[item.UID] = item
	}
	merged, err := projectmodel.Merge(agentID, fsFacts, dockerFacts, nil)
	if err != nil {
		return nil, err
	}
	result := make([]mergedProjectSnapshot, 0, len(merged))
	dockerServices := make(map[string][]string)
	for _, fact := range dockerFacts {
		if fact.WorkingDir != "" && fact.Service != "" {
			dockerServices[fact.WorkingDir] = append(dockerServices[fact.WorkingDir], fact.Service)
		}
	}
	for _, project := range merged {
		// A container with no working_dir label is intentionally not assigned a
		// synthetic project UID. It remains an inventory observation instead of
		// becoming a mutable project with a fabricated identity.
		if project.UID == "" {
			continue
		}
		item, found := byUID[project.UID]
		if found {
			item.Name = project.Name
			item.Services = append([]string(nil), project.Services...)
		} else {
			item = validatedProjectSnapshot{agentProjectSnapshot: agentProjectSnapshot{
				UID: project.UID, WorkingDir: project.WorkingDir, Name: project.Name,
				Services: append([]string(nil), project.Services...),
			}}
		}
		result = append(result, mergedProjectSnapshot{
			snapshot: item, managed: project.Managed, unmanagedReason: project.UnmanagedReason,
			containerIDs: append([]string(nil), project.ContainerIDs...), services: sortedProjectStrings(dockerServices[project.WorkingDir]),
			collision: project.NameCollision, includedBy: append([]string(nil), project.IncludedBy...),
		})
	}
	return result, nil
}

func sortedProjectStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (b *Backend) mergeProjectSnapshot(ctx context.Context, agentID string, status agentProjectScanStatus, projects []validatedProjectSnapshot) error {
	return b.mergeProjectSnapshotObserved(ctx, agentID, status, projects, time.Now().UTC())
}

func (b *Backend) mergeProjectSnapshotObserved(ctx context.Context, agentID string, status agentProjectScanStatus, projects []validatedProjectSnapshot, observedAt time.Time) error {
	return b.mergeProjectSnapshotWithDockerObserved(ctx, agentID, status, projects, nil, observedAt)
}

func (b *Backend) mergeProjectSnapshotWithDockerObserved(ctx context.Context, agentID string, status agentProjectScanStatus, projects []validatedProjectSnapshot, dockerFacts []projectmodel.DockerFact, observedAt time.Time) (err error) {
	defer func() { err = classifyStoreBusy(err) }()
	if observedAt.IsZero() {
		return errors.New("serverapi: project snapshot observation time is required")
	}
	observedAt = observedAt.UTC()
	merged, err := mergeProjectFacts(agentID, projects, dockerFacts)
	if err != nil {
		return &corruptDataError{boundary: "Agent project merge facts", cause: err}
	}
	// Serialize durable mirror transactions with operation recovery. The write
	// pool already makes the upgrade race impossible; this keeps unrelated
	// Agents from queueing on each other inside SQLite.
	if err := b.lockOperationMerge(ctx); err != nil {
		return err
	}
	defer b.unlockOperationMerge()
	tx, err := b.store.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("serverapi: begin project reconciliation: %w", err)
	}
	defer tx.Rollback()
	status.ScannedAt = status.ScannedAt.UTC()
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("serverapi: encode project scan status: %w", err)
	}
	defer clear(statusJSON)
	scannedAt := status.ScannedAt.Format(projectScanTimeFormat)
	claim, err := tx.ExecContext(ctx, `
		UPDATE agents SET projects_scanned_at = ?, project_scan_status_json = ?
		WHERE id = ? AND retired_at IS NULL
		  AND (projects_scanned_at IS NULL OR projects_scanned_at < ?)
	`, scannedAt, string(statusJSON), agentID, scannedAt)
	if err != nil {
		return fmt.Errorf("serverapi: claim project snapshot revision: %w", err)
	}
	affected, err := claim.RowsAffected()
	if err != nil {
		return fmt.Errorf("serverapi: inspect project snapshot claim: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id = ? AND retired_at IS NULL)`, agentID).Scan(&exists); err != nil {
			return fmt.Errorf("serverapi: inspect project snapshot Agent: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: Agent %q", webui.ErrNotFound, agentID)
		}
		return nil // Equal or older Agent snapshots never roll the mirror back.
	}

	type existingProject struct {
		workingDir, name, appliedJSON string
		flags                         projectFlags
	}
	existing := make(map[string]existingProject)
	rows, err := tx.QueryContext(ctx, `
		SELECT project_uid, working_dir, name, applied_fingerprints_json, flags_json
		FROM projects WHERE agent_id = ?
	`, agentID)
	if err != nil {
		return fmt.Errorf("serverapi: load prior project mirror: %w", err)
	}
	for rows.Next() {
		var uid, rawFlags string
		var item existingProject
		if err := rows.Scan(&uid, &item.workingDir, &item.name, &item.appliedJSON, &rawFlags); err != nil {
			rows.Close()
			return fmt.Errorf("serverapi: scan prior project mirror: %w", err)
		}
		if err := decodeStrictJSON([]byte(rawFlags), &item.flags); err != nil {
			rows.Close()
			return &corruptDataError{boundary: "projects.flags_json", cause: err}
		}
		existing[uid] = item
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("serverapi: close prior project rows: %w", err)
	}

	seen := make(map[string]struct{}, len(merged))
	for _, mergedItem := range merged {
		item := mergedItem.snapshot
		prior, found := existing[item.UID]
		if found && prior.workingDir != item.WorkingDir {
			return &corruptDataError{boundary: "projects identity", cause: errors.New("project UID changed working directory")}
		}
		if found {
			newer, err := isNewerProjectObservation(prior.flags.LastObservedAt, observedAt)
			if err != nil {
				return &corruptDataError{boundary: "projects.flags_json", cause: err}
			}
			if !newer {
				// The Agent response was requested before a targeted post-operation
				// refresh and arrived late. Preserve the newer project observation.
				seen[item.UID] = struct{}{}
				continue
			}
		}
		applied := ""
		if found {
			applied, err = appliedFingerprint(prior.appliedJSON)
			if err != nil {
				return &corruptDataError{boundary: "projects.applied_fingerprints_json", cause: err}
			}
		}
		flags := projectFlags{
			Managed:                 mergedItem.managed,
			UnmanagedReason:         mergedItem.unmanagedReason,
			ContainerIDs:            append([]string(nil), mergedItem.containerIDs...),
			Services:                append([]string(nil), mergedItem.services...),
			IncludedBy:              append([]string(nil), mergedItem.includedBy...),
			IncludedWorkDirs:        append([]string(nil), item.IncludedWorkDirs...),
			SourceReferences:        append([]agentSourceReference(nil), item.SourceReferences...),
			SourceGraphComplete:     item.SourceGraphComplete,
			Collision:               mergedItem.collision,
			Stale:                   item.Stale,
			ComposeExecutable:       item.ComposeExecutable,
			FilesystemWritable:      item.FilesystemWritable,
			RestoreRecoveryRequired: item.RestoreRecoveryRequired,
			CapabilityReason:        item.CapabilityReason,
			CurrentFingerprint:      item.CurrentFingerprint,
			LastVerifiedFingerprint: item.CurrentFingerprint,
			LastObservedAt:          observedAt.Format(time.RFC3339Nano),
			Drift:                   string(projectmodel.DriftStatus(applied, item.CurrentFingerprint)),
		}
		if !item.verifiedAt.IsZero() {
			flags.LastVerifiedAt = item.verifiedAt.Format(time.RFC3339Nano)
		}
		name := item.Name
		if item.Stale && found {
			flags.CurrentFingerprint = prior.flags.CurrentFingerprint
			flags.LastVerifiedFingerprint = prior.flags.LastVerifiedFingerprint
			flags.LastVerifiedAt = prior.flags.LastVerifiedAt
			flags.Drift = prior.flags.Drift
			if prior.name != "" {
				name = prior.name
			}
		}
		flags.ReadOnly = projectReadOnly(flags, flags.Collision)
		rawFlags, err := json.Marshal(flags)
		if err != nil {
			return fmt.Errorf("serverapi: encode project flags: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO projects(project_uid, agent_id, working_dir, name, applied_fingerprints_json, flags_json, updated_at)
			VALUES(?, ?, ?, ?, '[]', ?, ?)
			ON CONFLICT(project_uid) DO UPDATE SET
				name = CASE WHEN excluded.name != '' THEN excluded.name ELSE projects.name END,
				flags_json = excluded.flags_json, updated_at = excluded.updated_at
			WHERE projects.agent_id = excluded.agent_id AND projects.working_dir = excluded.working_dir
		`, item.UID, agentID, item.WorkingDir, name, string(rawFlags), scannedAt)
		clear(rawFlags)
		if err != nil {
			return fmt.Errorf("serverapi: merge project mirror: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return &corruptDataError{boundary: "projects identity", cause: errors.New("project UID belongs to different Agent identity")}
		}
		seen[item.UID] = struct{}{}
	}

	if !status.Truncated {
		for uid, prior := range existing {
			if _, present := seen[uid]; present {
				continue
			}
			flags := prior.flags
			flags.Missing = true
			flags.Stale = true
			flags.ReadOnly = true
			flags.Collision = false
			flags.ComposeExecutable = false
			flags.FilesystemWritable = false
			flags.CapabilityReason = "project missing from latest complete discovery scan"
			flags.IncludedBy = nil
			rawFlags, err := json.Marshal(flags)
			if err != nil {
				return fmt.Errorf("serverapi: encode missing project flags: %w", err)
			}
			_, err = tx.ExecContext(ctx, `UPDATE projects SET flags_json = ?, updated_at = ? WHERE project_uid = ? AND agent_id = ?`, string(rawFlags), scannedAt, uid, agentID)
			clear(rawFlags)
			if err != nil {
				return fmt.Errorf("serverapi: preserve missing project history: %w", err)
			}
		}
	}
	if err := reconcileProjectIncludeRelations(ctx, tx, agentID, scannedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("serverapi: commit project reconciliation: %w", err)
	}
	return nil
}

// mergeTargetedProjectSnapshot updates only the project freshly rescanned by
// the Agent. The mirror row, its observed fingerprint, and a compose.up
// baseline are committed together so a baseline can never point at a different
// Agent, project, or current fingerprint.
func (b *Backend) mergeTargetedProjectSnapshot(ctx context.Context, agentID string, item validatedProjectSnapshot, establishBaseline bool) error {
	return b.mergeTargetedProjectSnapshotObserved(ctx, agentID, item, establishBaseline, time.Now().UTC())
}

func (b *Backend) mergeTargetedProjectSnapshotObserved(ctx context.Context, agentID string, item validatedProjectSnapshot, establishBaseline bool, observedAt time.Time) (err error) {
	defer func() { err = classifyStoreBusy(err) }()
	if observedAt.IsZero() {
		return errors.New("serverapi: targeted project observation time is required")
	}
	observedAt = observedAt.UTC()
	if err := b.lockOperationMerge(ctx); err != nil {
		return err
	}
	defer b.unlockOperationMerge()
	tx, err := b.store.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("serverapi: begin targeted project reconciliation: %w", err)
	}
	defer tx.Rollback()
	var workingDir, priorName, appliedJSON, rawFlags string
	err = tx.QueryRowContext(ctx, `
		SELECT working_dir, name, applied_fingerprints_json, flags_json
		FROM projects WHERE project_uid = ? AND agent_id = ?
	`, item.UID, agentID).Scan(&workingDir, &priorName, &appliedJSON, &rawFlags)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %q is not present in the Server mirror", webui.ErrConflict, item.UID)
	}
	if err != nil {
		return fmt.Errorf("serverapi: load targeted project mirror: %w", err)
	}
	if workingDir != item.WorkingDir {
		return &corruptDataError{boundary: "projects identity", cause: errors.New("targeted project working directory changed")}
	}
	var prior projectFlags
	if err := decodeStrictJSON([]byte(rawFlags), &prior); err != nil {
		return &corruptDataError{boundary: "projects.flags_json", cause: err}
	}
	applied, err := appliedFingerprint(appliedJSON)
	if err != nil {
		return &corruptDataError{boundary: "projects.applied_fingerprints_json", cause: err}
	}
	if establishBaseline {
		// validateTargetedProjectSnapshot has already recomputed and checked this
		// fingerprint from the complete received file-fact set.
		applied = item.CurrentFingerprint
	}
	flags := projectFlags{
		Managed:                 prior.Managed,
		UnmanagedReason:         prior.UnmanagedReason,
		ContainerIDs:            append([]string(nil), prior.ContainerIDs...),
		Services:                append([]string(nil), prior.Services...),
		IncludedBy:              append([]string(nil), prior.IncludedBy...),
		IncludedWorkDirs:        append([]string(nil), item.IncludedWorkDirs...),
		SourceReferences:        append([]agentSourceReference(nil), item.SourceReferences...),
		SourceGraphComplete:     item.SourceGraphComplete,
		Stale:                   item.Stale,
		ComposeExecutable:       item.ComposeExecutable,
		FilesystemWritable:      item.FilesystemWritable,
		CapabilityReason:        item.CapabilityReason,
		CurrentFingerprint:      item.CurrentFingerprint,
		LastVerifiedFingerprint: item.CurrentFingerprint,
		LastObservedAt:          observedAt.Format(time.RFC3339Nano),
		Drift:                   string(projectmodel.DriftStatus(applied, item.CurrentFingerprint)),
	}
	if item.Stale {
		flags.CurrentFingerprint = prior.CurrentFingerprint
		flags.LastVerifiedFingerprint = prior.LastVerifiedFingerprint
		flags.LastVerifiedAt = prior.LastVerifiedAt
		flags.Drift = prior.Drift
	} else {
		flags.LastVerifiedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	flags.ReadOnly = projectReadOnly(flags, flags.Collision)
	name := item.Name
	if name == "" {
		name = priorName
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	appliedBytes, err := marshalAppliedFingerprint(applied)
	if err != nil {
		return err
	}
	defer clear(appliedBytes)
	rawUpdatedFlags, err := json.Marshal(flags)
	if err != nil {
		return fmt.Errorf("serverapi: encode targeted project flags: %w", err)
	}
	defer clear(rawUpdatedFlags)
	result, err := tx.ExecContext(ctx, `
		UPDATE projects SET name = ?, applied_fingerprints_json = ?, flags_json = ?, updated_at = ?
		WHERE project_uid = ? AND agent_id = ? AND working_dir = ?
	`, name, string(appliedBytes), string(rawUpdatedFlags), updatedAt, item.UID, agentID, item.WorkingDir)
	if err != nil {
		return fmt.Errorf("serverapi: merge targeted project snapshot: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return &corruptDataError{boundary: "projects identity", cause: errors.New("targeted project changed concurrently")}
	}
	if err := reconcileProjectIncludeRelations(ctx, tx, agentID, updatedAt); err != nil {
		return err
	}
	if err := reconcileTargetedProjectCollisions(ctx, tx, agentID, updatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("serverapi: commit targeted project reconciliation: %w", err)
	}
	return nil
}

func isNewerProjectObservation(previous string, current time.Time) (bool, error) {
	if previous == "" {
		return true, nil
	}
	observed, err := time.Parse(time.RFC3339Nano, previous)
	if err != nil || observed.IsZero() {
		return false, errors.New("invalid last observed timestamp")
	}
	return current.After(observed), nil
}

func marshalAppliedFingerprint(fingerprint string) ([]byte, error) {
	if fingerprint == "" {
		return []byte("[]"), nil
	}
	raw, err := json.Marshal(storedAppliedFingerprint{Version: 1, Fingerprint: fingerprint})
	if err != nil {
		return nil, fmt.Errorf("serverapi: encode applied fingerprint: %w", err)
	}
	return raw, nil
}

// reconcileProjectIncludeRelations derives UI nesting from the current
// project mirror after either a full or targeted Agent observation. The Agent
// supplies only raw include directories; Server-side stable UIDs decide the
// final parent links so an unknown/missing directory never gains a synthetic
// project identity.
func reconcileProjectIncludeRelations(ctx context.Context, tx *sql.Tx, agentID, updatedAt string) error {
	type entry struct {
		uid        string
		workingDir string
		flags      projectFlags
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_uid, working_dir, flags_json FROM projects WHERE agent_id = ?`, agentID)
	if err != nil {
		return fmt.Errorf("serverapi: load project include relations: %w", err)
	}
	defer rows.Close()
	entries := make([]entry, 0)
	byDirectory := make(map[string]int)
	for rows.Next() {
		var item entry
		var raw string
		if err := rows.Scan(&item.uid, &item.workingDir, &raw); err != nil {
			return fmt.Errorf("serverapi: scan project include relation: %w", err)
		}
		if item.uid == "" || !utf8.ValidString(item.uid) || !canonicalAbsolutePath(item.workingDir) {
			return &corruptDataError{boundary: "projects include relation", cause: errors.New("invalid project identity")}
		}
		if err := decodeStrictJSON([]byte(raw), &item.flags); err != nil {
			return &corruptDataError{boundary: "projects.flags_json", cause: err}
		}
		for _, directory := range item.flags.IncludedWorkDirs {
			if !canonicalAbsolutePath(directory) {
				return &corruptDataError{boundary: "projects.flags_json", cause: errors.New("invalid included working directory")}
			}
		}
		if _, duplicate := byDirectory[item.workingDir]; duplicate {
			return &corruptDataError{boundary: "projects include relation", cause: errors.New("duplicate project working directory")}
		}
		byDirectory[item.workingDir] = len(entries)
		entries = append(entries, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("serverapi: iterate project include relations: %w", err)
	}
	parents := make([][]string, len(entries))
	for parentIndex, parent := range entries {
		if parent.flags.Missing {
			continue
		}
		for _, directory := range parent.flags.IncludedWorkDirs {
			childIndex, found := byDirectory[directory]
			if !found || childIndex == parentIndex || entries[childIndex].flags.Missing {
				continue
			}
			parents[childIndex] = append(parents[childIndex], parent.uid)
		}
	}
	for index := range entries {
		next := sortedProjectStrings(parents[index])
		if entries[index].flags.Missing {
			next = nil
		}
		if equalProjectStrings(entries[index].flags.IncludedBy, next) {
			continue
		}
		entries[index].flags.IncludedBy = next
		raw, err := json.Marshal(entries[index].flags)
		if err != nil {
			return fmt.Errorf("serverapi: encode project include relation: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET flags_json = ?, updated_at = ? WHERE project_uid = ? AND agent_id = ?`, string(raw), updatedAt, entries[index].uid, agentID)
		clear(raw)
		if err != nil {
			return fmt.Errorf("serverapi: update project include relation: %w", err)
		}
	}
	return nil
}

func equalProjectStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// projectReadOnly is the single definition of "this project cannot be changed".
// It had been written out at four call sites, and adding a reason to one of
// them left the other three able to clear the bit again - which is how a
// recovery-blocked project could be advertised as writable moments after being
// reported as damaged.
func projectReadOnly(flags projectFlags, collision bool) bool {
	return !managedProject(flags) || flags.Stale || !flags.ComposeExecutable ||
		!flags.FilesystemWritable || collision || flags.RestoreRecoveryRequired
}

func reconcileTargetedProjectCollisions(ctx context.Context, tx *sql.Tx, agentID, updatedAt string) error {
	type entry struct {
		uid, name string
		flags     projectFlags
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_uid, name, flags_json FROM projects WHERE agent_id = ?`, agentID)
	if err != nil {
		return fmt.Errorf("serverapi: load project collisions: %w", err)
	}
	defer rows.Close()
	entries := make([]entry, 0)
	counts := make(map[string]int)
	for rows.Next() {
		var value entry
		var raw string
		if err := rows.Scan(&value.uid, &value.name, &raw); err != nil {
			return fmt.Errorf("serverapi: scan project collision: %w", err)
		}
		if err := decodeStrictJSON([]byte(raw), &value.flags); err != nil {
			return &corruptDataError{boundary: "projects.flags_json", cause: err}
		}
		entries = append(entries, value)
		if !value.flags.Missing && value.name != "" {
			counts[value.name]++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("serverapi: iterate project collisions: %w", err)
	}
	for _, value := range entries {
		collision := !value.flags.Missing && value.name != "" && counts[value.name] > 1
		if value.flags.Collision == collision && value.flags.ReadOnly == projectReadOnly(value.flags, collision) {
			continue
		}
		value.flags.Collision = collision
		value.flags.ReadOnly = projectReadOnly(value.flags, collision)
		raw, err := json.Marshal(value.flags)
		if err != nil {
			return fmt.Errorf("serverapi: encode project collision flags: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET flags_json = ?, updated_at = ? WHERE project_uid = ? AND agent_id = ?`, string(raw), updatedAt, value.uid, agentID)
		clear(raw)
		if err != nil {
			return fmt.Errorf("serverapi: update project collision flags: %w", err)
		}
	}
	return nil
}

func managedProject(flags projectFlags) bool {
	return flags.Managed || flags.UnmanagedReason == ""
}

func appliedFingerprint(raw string) (string, error) {
	if strings.TrimSpace(raw) == "[]" {
		return "", nil
	}
	var value storedAppliedFingerprint
	if err := decodeStrictJSON([]byte(raw), &value); err != nil {
		return "", err
	}
	if value.Version != 1 || value.Fingerprint != "" && !canonicalSHA256.MatchString(value.Fingerprint) {
		return "", errors.New("invalid applied fingerprint")
	}
	return value.Fingerprint, nil
}

func canonicalAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsRune(value, 0)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validProjectScanStopReason(value string) bool {
	switch value {
	case "MAX_DIRECTORIES", "MAX_DURATION", "CONTEXT_CANCELED", "FILESYSTEM_ERROR", "UNSAFE_PATH", "FILE_UNSTABLE":
		return true
	default:
		return false
	}
}
