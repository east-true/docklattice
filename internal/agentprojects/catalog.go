// Package agentprojects keeps the Agent's bounded in-memory mapping from a
// Server-selected project UID to verified Compose execution paths. Durable
// project identity/merge/drift state remains Server-owned.
package agentprojects

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/backup"
	"github.com/east-true/dockpilot/internal/composeconfig"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/composesource"
	"github.com/east-true/dockpilot/internal/discovery"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/safefile"
)

type Scanner interface {
	Scan(context.Context) (discovery.Result, error)
}

type ProjectScanner interface {
	ScanProject(context.Context, string, string) ([]discovery.File, error)
}

type Evaluator interface {
	Evaluate(context.Context, string, []string) (composeconfig.Result, error)
}

// SourceGraph extracts source provenance only. It must not derive Compose
// names, services, merged config, or execution input; Evaluator remains the
// Docker Compose CLI boundary for those semantics.
type SourceGraph interface {
	Analyze(context.Context, string, string, []string) (composesource.Result, error)
}

type RootExecutionPolicy func(root string) (allowed bool, reason string)

type Project struct {
	UID          string
	Root         string
	WorkingDir   string
	Files        []projectmodel.FileFact
	ComposeFiles []string
	Name         string
	Services     []string
	// EnvFiles is metadata only: it records service env_file references from
	// Compose's resolved model without retaining any environment value or file
	// content. Readable is true only for a current read-only safefile approval.
	EnvFiles            []EnvFileReference
	SourceReferences    []SourceReference
	ReadOnlyFiles       []safefile.ApprovedFile
	IncludedWorkDirs    []string
	SourceGraphComplete bool
	CurrentFingerprint  string
	ComposeExecutable   bool
	FilesystemWritable  bool
	CapabilityReason    string
	Stale               bool
}

type EnvFileReference struct {
	Path     string
	Readable bool
}

// SourceReference is content-free include/extends provenance. ReadOnly is
// true only when this project safely read the source and granted a temporary
// safefile approval; outside-working-dir references can be Accessible but are
// deliberately never readable through this project.
type SourceReference struct {
	Kind       string
	Path       string
	Accessible bool
	ReadOnly   bool
}

type ScanStatus struct {
	ScannedAt       time.Time
	Truncated       bool
	StopReason      discovery.StopReason
	DirectoriesSeen int
	LastScannedPath string
}

// ExternalConfigChange is a bounded, content-free summary of one managed
// project's key-file change observed by a periodic discovery scan. The report
// deliberately carries neither file paths, hashes, nor contents into Audit.
type ExternalConfigChange struct {
	ProjectUID       string
	ChangedFileCount int
	ObservedAt       time.Time
}

// ExternalChangeObserver durably records a periodic external configuration
// observation before Catalog advances its comparison point. Returning an
// error leaves the prior catalog intact so a later scan can retry the same
// observation instead of silently losing it.
type ExternalChangeObserver func(context.Context, []ExternalConfigChange) error

type Catalog struct {
	agentID     string
	scanner     Scanner
	evaluator   Evaluator
	sourceGraph SourceGraph
	policy      RootExecutionPolicy
	writePolicy RootExecutionPolicy
	now         func() time.Time

	scanMu   sync.Mutex
	mu       sync.RWMutex
	projects map[string]Project
	status   ScanStatus
}

var _ backup.ProjectResolver = (*Catalog)(nil)

func New(agentID string, scanner Scanner, evaluator Evaluator, policy RootExecutionPolicy) (*Catalog, error) {
	return NewWithPolicies(agentID, scanner, evaluator, policy, policy)
}

func NewWithPolicies(agentID string, scanner Scanner, evaluator Evaluator, policy, writePolicy RootExecutionPolicy) (*Catalog, error) {
	return NewWithSourceGraph(agentID, scanner, evaluator, nil, policy, writePolicy)
}

// NewWithSourceGraph installs the optional narrow source-provenance parser.
// Passing nil preserves the legacy test seam; production always supplies the
// bounded implementation from composesource.
func NewWithSourceGraph(agentID string, scanner Scanner, evaluator Evaluator, sourceGraph SourceGraph, policy, writePolicy RootExecutionPolicy) (*Catalog, error) {
	if agentID == "" || scanner == nil || evaluator == nil || policy == nil || writePolicy == nil {
		return nil, errors.New("agentprojects: Agent ID, scanner, evaluator, and root policy are required")
	}
	return &Catalog{agentID: agentID, scanner: scanner, evaluator: evaluator, sourceGraph: sourceGraph, policy: policy, writePolicy: writePolicy, now: time.Now, projects: make(map[string]Project)}, nil
}

// Rescan hashes the filesystem first, and invokes Compose only when a
// directory fingerprint changed. A truncated scan keeps previously observed
// but unseen projects as stale rather than silently deleting them. It is used
// for boot, user-requested, and managed post-operation refreshes, none of
// which are external-change observations.
func (c *Catalog) Rescan(ctx context.Context) error {
	return c.rescan(ctx, nil)
}

// RescanForExternalChanges is the periodic polling path from architecture
// section 7.7. It compares the new verified key-file hashes with the prior
// catalog and invokes observer before publishing the new comparison point.
// Targeted post-operation refreshes intentionally use RescanProject instead,
// so Dockpilot's own successful writes never become OBSERVED events.
func (c *Catalog) RescanForExternalChanges(ctx context.Context, observer ExternalChangeObserver) error {
	if observer == nil {
		return errors.New("agentprojects: external change observer is required")
	}
	return c.rescan(ctx, observer)
}

func (c *Catalog) rescan(ctx context.Context, observer ExternalChangeObserver) error {
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	result, err := c.scanner.Scan(ctx)
	if err != nil && !result.Truncated {
		return err
	}
	grouped := make(map[string][]discovery.File)
	for _, file := range result.Files {
		grouped[filepath.Dir(file.Path)] = append(grouped[filepath.Dir(file.Path)], file)
	}

	c.mu.RLock()
	previous := cloneProjects(c.projects)
	c.mu.RUnlock()
	next := make(map[string]Project, len(grouped)+len(previous))
	var evaluationErr error
	for directory, files := range grouped {
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		facts := make([]projectmodel.FileFact, 0, len(files))
		paths := make([]string, 0, len(files))
		root := ""
		for _, file := range files {
			if root == "" {
				root = file.Root
			}
			facts = append(facts, projectmodel.FileFact{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
			if file.Kind == "" || file.Kind == discovery.FileKindCompose {
				paths = append(paths, file.Path)
			}
		}
		uid, uidErr := projectmodel.UID(c.agentID, directory)
		if uidErr != nil {
			return uidErr
		}
		if len(paths) == 0 {
			return fmt.Errorf("agentprojects: project %q has no Compose configuration files", directory)
		}
		project := Project{UID: uid, Root: root, WorkingDir: directory, Files: facts, ComposeFiles: paths, SourceGraphComplete: true}
		project.ComposeExecutable, project.CapabilityReason = c.policy(root)
		project.FilesystemWritable, _ = c.writePolicy(root)
		if !project.ComposeExecutable {
			fingerprint, fingerprintErr := projectmodel.Fingerprint(project.Files)
			if fingerprintErr != nil {
				return fingerprintErr
			}
			project.CurrentFingerprint = fingerprint
			next[uid] = project
			continue
		}
		project.Files, project.SourceReferences, project.ReadOnlyFiles, project.IncludedWorkDirs, project.SourceGraphComplete =
			c.sourceGraphFacts(ctx, project.Root, project.WorkingDir, project.ComposeFiles, project.Files)
		fingerprint, fingerprintErr := projectmodel.Fingerprint(project.Files)
		if fingerprintErr != nil {
			return fingerprintErr
		}
		project.CurrentFingerprint = fingerprint
		if cached, ok := previous[uid]; ok && cached.CurrentFingerprint == fingerprint && cached.ComposeExecutable &&
			cached.SourceGraphComplete && project.SourceGraphComplete {
			project.Name = cached.Name
			project.Services = append([]string(nil), cached.Services...)
			project.EnvFiles = append([]EnvFileReference(nil), cached.EnvFiles...)
			project.ReadOnlyFiles = append([]safefile.ApprovedFile(nil), cached.ReadOnlyFiles...)
			next[uid] = project
			continue
		}
		resolved, evaluateErr := c.evaluator.Evaluate(ctx, directory, paths)
		if evaluateErr != nil {
			// Hashes were obtained safely before Compose evaluation. Preserve this
			// current filesystem observation for drift/audit while failing closed
			// for execution; otherwise an externally broken configuration would
			// be invisible until Compose became valid again.
			project.ComposeExecutable = false
			project.CapabilityReason = "Compose configuration evaluation failed"
			next[uid] = project
			evaluationErr = errors.Join(evaluationErr, fmt.Errorf("agentprojects: evaluate %q: %w", directory, evaluateErr))
			continue
		}
		if resolved.Project.WorkingDir != directory || resolved.Project.Name == "" ||
			!equalStrings(resolved.Project.Files, paths) {
			return fmt.Errorf("agentprojects: evaluator changed verified project identity for %q", directory)
		}
		project.Name = resolved.Project.Name
		project.Services = append([]string(nil), resolved.Services...)
		sourceApprovals := append([]safefile.ApprovedFile(nil), project.ReadOnlyFiles...)
		var envApprovals []safefile.ApprovedFile
		project.EnvFiles, envApprovals = classifyEnvFileReferences(ctx, project.Root, project.WorkingDir, resolved.EnvFiles)
		project.ReadOnlyFiles = mergeReadOnlyApprovals(sourceApprovals, envApprovals)
		next[uid] = project
	}
	if result.Truncated {
		for uid, project := range previous {
			if _, seen := next[uid]; seen {
				continue
			}
			project.Stale = true
			project.ComposeExecutable = false
			project.CapabilityReason = "discovery scan was truncated before this project was revisited"
			next[uid] = project
		}
	}
	status := ScanStatus{
		ScannedAt: c.now().UTC(), Truncated: result.Truncated, StopReason: result.StopReason,
		DirectoriesSeen: result.DirectoriesSeen, LastScannedPath: result.LastScannedPath,
	}
	if observer != nil {
		changes, changesErr := externalConfigChanges(previous, next, result.Truncated, status.ScannedAt)
		if changesErr != nil {
			return changesErr
		}
		if len(changes) != 0 {
			if observeErr := observer(ctx, changes); observeErr != nil {
				return fmt.Errorf("agentprojects: record external configuration changes: %w", observeErr)
			}
		}
	}
	c.mu.Lock()
	c.projects = next
	c.status = status
	c.mu.Unlock()
	return errors.Join(err, evaluationErr)
}

func externalConfigChanges(previous, next map[string]Project, truncated bool, observedAt time.Time) ([]ExternalConfigChange, error) {
	projectUIDs := make([]string, 0, len(previous))
	for uid := range previous {
		projectUIDs = append(projectUIDs, uid)
	}
	sort.Strings(projectUIDs)
	changes := make([]ExternalConfigChange, 0)
	for _, uid := range projectUIDs {
		before := previous[uid]
		if !externallyManaged(before) {
			continue
		}
		after, found := next[uid]
		if !found {
			// A truncated global walk cannot prove a previously seen project was
			// removed. A complete walk can report its tracked key files as gone.
			if !truncated {
				changes = append(changes, ExternalConfigChange{
					ProjectUID: uid, ChangedFileCount: len(before.Files), ObservedAt: observedAt,
				})
			}
			continue
		}
		if after.Stale {
			continue
		}
		changedFiles, err := projectmodel.ChangedFiles(before.Files, after.Files)
		if err != nil {
			return nil, fmt.Errorf("agentprojects: compare project %q key files: %w", uid, err)
		}
		if len(changedFiles) != 0 {
			changes = append(changes, ExternalConfigChange{
				ProjectUID: uid, ChangedFileCount: len(changedFiles), ObservedAt: observedAt,
			})
		}
	}
	return changes, nil
}

func externallyManaged(project Project) bool {
	return !project.Stale && project.ComposeExecutable
}

// RescanProject refreshes exactly one project after a successful managed
// change. It never invokes the global discovery walk, so an operation cannot
// accidentally turn a targeted postcondition into host-wide I/O.
func (c *Catalog) RescanProject(ctx context.Context, uid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scanner, ok := c.scanner.(ProjectScanner)
	if !ok {
		return errors.New("agentprojects: scanner does not support targeted project refresh")
	}
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	c.mu.RLock()
	previous, exists := c.projects[uid]
	c.mu.RUnlock()
	if !exists || previous.Stale {
		return fmt.Errorf("agentprojects: project %q is unavailable for targeted refresh", uid)
	}
	files, err := scanner.ScanProject(ctx, previous.Root, previous.WorkingDir)
	if err != nil {
		return fmt.Errorf("agentprojects: refresh project %q: %w", uid, err)
	}
	project, err := c.projectFromFiles(ctx, previous, files)
	if err != nil {
		return err
	}
	c.mu.Lock()
	// A full scan cannot race this replacement: scanMu serializes discovery
	// publications. Keep every unrelated project exactly as it was.
	c.projects[uid] = project
	c.mu.Unlock()
	return nil
}

// ProjectSnapshot returns the current verified catalog entry for one stable
// UID. It is intentionally separate from Project, which exposes only the
// minimal Compose execution input.
func (c *Catalog) ProjectSnapshot(uid string) (Project, bool) {
	c.mu.RLock()
	project, ok := c.projects[uid]
	c.mu.RUnlock()
	if !ok {
		return Project{}, false
	}
	return cloneProject(project), true
}

func (c *Catalog) projectFromFiles(ctx context.Context, previous Project, files []discovery.File) (Project, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	facts := make([]projectmodel.FileFact, 0, len(files))
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if file.Root != previous.Root || filepath.Dir(file.Path) != previous.WorkingDir {
			return Project{}, fmt.Errorf("agentprojects: targeted refresh changed verified project identity for %q", previous.WorkingDir)
		}
		facts = append(facts, projectmodel.FileFact{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
		if file.Kind == "" || file.Kind == discovery.FileKindCompose {
			paths = append(paths, file.Path)
		}
	}
	if len(paths) == 0 {
		return Project{}, fmt.Errorf("agentprojects: project %q has no Compose configuration files", previous.WorkingDir)
	}
	project := Project{
		UID: previous.UID, Root: previous.Root, WorkingDir: previous.WorkingDir,
		Files: facts, ComposeFiles: paths, SourceGraphComplete: true,
	}
	project.ComposeExecutable, project.CapabilityReason = c.policy(project.Root)
	project.FilesystemWritable, _ = c.writePolicy(project.Root)
	if !project.ComposeExecutable {
		fingerprint, fingerprintErr := projectmodel.Fingerprint(project.Files)
		if fingerprintErr != nil {
			return Project{}, fingerprintErr
		}
		project.CurrentFingerprint = fingerprint
		project.Name = previous.Name
		project.Services = append([]string(nil), previous.Services...)
		return project, nil
	}
	project.Files, project.SourceReferences, project.ReadOnlyFiles, project.IncludedWorkDirs, project.SourceGraphComplete =
		c.sourceGraphFacts(ctx, project.Root, project.WorkingDir, project.ComposeFiles, project.Files)
	fingerprint, fingerprintErr := projectmodel.Fingerprint(project.Files)
	if fingerprintErr != nil {
		return Project{}, fingerprintErr
	}
	project.CurrentFingerprint = fingerprint
	if previous.CurrentFingerprint == fingerprint && previous.ComposeExecutable && previous.Name != "" &&
		previous.SourceGraphComplete && project.SourceGraphComplete {
		project.Name = previous.Name
		project.Services = append([]string(nil), previous.Services...)
		project.EnvFiles = append([]EnvFileReference(nil), previous.EnvFiles...)
		project.ReadOnlyFiles = append([]safefile.ApprovedFile(nil), previous.ReadOnlyFiles...)
		return project, nil
	}
	resolved, err := c.evaluator.Evaluate(ctx, project.WorkingDir, project.ComposeFiles)
	if err != nil {
		return Project{}, fmt.Errorf("agentprojects: evaluate %q: %w", project.WorkingDir, err)
	}
	if resolved.Project.WorkingDir != project.WorkingDir || resolved.Project.Name == "" ||
		!equalStrings(resolved.Project.Files, project.ComposeFiles) {
		return Project{}, fmt.Errorf("agentprojects: evaluator changed verified project identity for %q", project.WorkingDir)
	}
	project.Name = resolved.Project.Name
	project.Services = append([]string(nil), resolved.Services...)
	sourceApprovals := append([]safefile.ApprovedFile(nil), project.ReadOnlyFiles...)
	var envApprovals []safefile.ApprovedFile
	project.EnvFiles, envApprovals = classifyEnvFileReferences(ctx, project.Root, project.WorkingDir, resolved.EnvFiles)
	project.ReadOnlyFiles = mergeReadOnlyApprovals(sourceApprovals, envApprovals)
	return project, nil
}

// classifyEnvFileReferences turns only Compose-resolved service env_file
// paths into temporary read-only safefile approvals. A project reaches this
// function only after its discovery root passed the Agent's mount/identity
// execution policy. The checks below add project-root and discovery-root
// containment, then safefile verifies every component and target with
// O_NOFOLLOW. Unsafe, absent, or out-of-root references remain metadata only.
func classifyEnvFileReferences(ctx context.Context, discoveryRoot, workingDir string, references []string) ([]EnvFileReference, []safefile.ApprovedFile) {
	metadata := make([]EnvFileReference, 0, len(references))
	type candidate struct {
		metadataIndices []int
		relativePath    string
	}
	candidates := make([]candidate, 0, len(references))
	byRelative := make(map[string]int)
	for _, reference := range references {
		candidatePath, valid := resolveEnvFilePath(workingDir, reference)
		metadata = append(metadata, EnvFileReference{Path: candidatePath})
		if !valid || !pathWithin(workingDir, candidatePath) || !pathWithin(discoveryRoot, candidatePath) {
			continue
		}
		relative, err := filepath.Rel(workingDir, candidatePath)
		if err != nil || relative == "." || filepath.IsAbs(relative) {
			continue
		}
		relative = filepath.ToSlash(relative)
		if index, duplicate := byRelative[relative]; duplicate {
			candidates[index].metadataIndices = append(candidates[index].metadataIndices, len(metadata)-1)
			continue
		}
		byRelative[relative] = len(candidates)
		candidates = append(candidates, candidate{metadataIndices: []int{len(metadata) - 1}, relativePath: relative})
	}
	if len(candidates) == 0 {
		return metadata, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].relativePath < candidates[j].relativePath })
	potential := make([]safefile.ApprovedFile, 0, len(candidates))
	for _, candidate := range candidates {
		potential = append(potential, safefile.ApprovedFile{RelativePath: candidate.relativePath, Access: safefile.ReadOnly})
	}
	root, err := safefile.OpenRoot(workingDir, potential)
	if err != nil {
		return metadata, nil
	}
	defer root.Close()
	approved := make([]safefile.ApprovedFile, 0, len(potential))
	for _, candidate := range candidates {
		if err := root.VerifyReadOnly(ctx, candidate.relativePath); err != nil {
			continue
		}
		for _, index := range candidate.metadataIndices {
			metadata[index].Readable = true
		}
		approved = append(approved, safefile.ApprovedFile{RelativePath: candidate.relativePath, Access: safefile.ReadOnly})
	}
	return metadata, approved
}

func resolveEnvFilePath(workingDir, reference string) (string, bool) {
	if reference == "" || strings.IndexByte(reference, 0) >= 0 || !filepath.IsAbs(workingDir) || filepath.Clean(workingDir) != workingDir {
		return reference, false
	}
	if filepath.IsAbs(reference) {
		return filepath.Clean(reference), true
	}
	return filepath.Clean(filepath.Join(workingDir, reference)), true
}

func (c *Catalog) sourceGraphFacts(ctx context.Context, discoveryRoot, workingDir string, composeFiles []string, facts []projectmodel.FileFact) ([]projectmodel.FileFact, []SourceReference, []safefile.ApprovedFile, []string, bool) {
	if c.sourceGraph == nil {
		return facts, nil, nil, nil, true
	}
	result, err := c.sourceGraph.Analyze(ctx, discoveryRoot, workingDir, composeFiles)
	if err != nil {
		// The source graph is provenance-only. Never replace Docker Compose's
		// evaluator with this parser; a failed graph merely makes cache reuse
		// unsafe until the next bounded scan can establish it again.
		return facts, nil, nil, nil, false
	}
	complete := result.Complete
	byPath := make(map[string]projectmodel.FileFact, len(facts)+len(result.Files))
	for _, fact := range facts {
		byPath[fact.Path] = fact
	}
	for _, source := range result.Files {
		if !pathWithin(workingDir, source.Path) || source.Size < 0 {
			complete = false
			continue
		}
		byPath[source.Path] = projectmodel.FileFact{Path: source.Path, Size: source.Size, SHA256: source.SHA256}
	}
	mergedFacts := make([]projectmodel.FileFact, 0, len(byPath))
	for _, fact := range byPath {
		mergedFacts = append(mergedFacts, fact)
	}
	sort.Slice(mergedFacts, func(left, right int) bool { return mergedFacts[left].Path < mergedFacts[right].Path })

	references := make([]SourceReference, 0, len(result.References))
	for _, reference := range result.References {
		if reference.Kind != composesource.KindInclude && reference.Kind != composesource.KindExtends ||
			!pathWithin(discoveryRoot, reference.Path) {
			complete = false
			continue
		}
		references = append(references, SourceReference{
			Kind: string(reference.Kind), Path: reference.Path, Accessible: reference.Accessible, ReadOnly: reference.ReadOnly,
		})
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].Kind != references[right].Kind {
			return references[left].Kind < references[right].Kind
		}
		return references[left].Path < references[right].Path
	})

	approvals := make([]safefile.ApprovedFile, 0, len(result.ReadOnlyPaths))
	for _, relative := range result.ReadOnlyPaths {
		candidate := filepath.Clean(filepath.Join(workingDir, filepath.FromSlash(relative)))
		if relative == "" || filepath.IsAbs(relative) || !pathWithin(workingDir, candidate) || candidate == workingDir {
			complete = false
			continue
		}
		approvals = append(approvals, safefile.ApprovedFile{RelativePath: filepath.ToSlash(relative), Access: safefile.ReadOnly})
	}
	approvals = mergeReadOnlyApprovals(approvals)
	return mergedFacts, references, approvals, append([]string(nil), result.IncludedWorkDirs...), complete
}

func mergeReadOnlyApprovals(groups ...[]safefile.ApprovedFile) []safefile.ApprovedFile {
	byPath := make(map[string]safefile.ApprovedFile)
	for _, group := range groups {
		for _, approval := range group {
			if approval.Access == safefile.ReadOnly && approval.RelativePath != "" {
				byPath[approval.RelativePath] = approval
			}
		}
	}
	result := make([]safefile.ApprovedFile, 0, len(byPath))
	for _, approval := range byPath {
		result = append(result, approval)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].RelativePath < result[right].RelativePath })
	return result
}

func pathWithin(root, candidate string) bool {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return false
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func equalStrings(left, right []string) bool {
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

// Project implements agentops.ProjectCatalog. Stale/read-only safety-degraded
// entries remain visible in Snapshot but can never reach Compose execution.
func (c *Catalog) Project(_ context.Context, uid string) (composeexec.Project, bool, error) {
	c.mu.RLock()
	project, ok := c.projects[uid]
	c.mu.RUnlock()
	if !ok || project.Stale || !project.ComposeExecutable || project.Name == "" {
		return composeexec.Project{}, false, nil
	}
	return composeexec.Project{WorkingDir: project.WorkingDir, Files: append([]string(nil), project.ComposeFiles...), Name: project.Name}, true, nil
}

// ApprovedReadOnlyFiles returns a defensive copy of the current catalog's
// env_file approvals. Callers may use it only as an additional safefile
// allowlist; absence never grants a broader default path.
func (c *Catalog) ApprovedReadOnlyFiles(ctx context.Context, uid string) ([]safefile.ApprovedFile, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.mu.RLock()
	project, ok := c.projects[uid]
	c.mu.RUnlock()
	if !ok || project.Stale || !project.ComposeExecutable {
		return nil, false, nil
	}
	return append([]safefile.ApprovedFile(nil), project.ReadOnlyFiles...), true, nil
}

// FilesystemMutationAllowed distinguishes a valid read-only identical-path
// bind from a writable identical-path bind without weakening Compose reads or
// lifecycle operations on read-only project configuration.
func (c *Catalog) FilesystemMutationAllowed(_ context.Context, uid string) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	project, ok := c.projects[uid]
	if !ok || project.Stale {
		return false, "project is unavailable or stale"
	}
	if !project.FilesystemWritable {
		return false, project.CapabilityReason
	}
	return true, ""
}

func (c *Catalog) Snapshot() ([]Project, ScanStatus) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Project, 0, len(c.projects))
	for _, project := range c.projects {
		result = append(result, cloneProject(project))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result, c.status
}

// ResolveBackupProject implements backup.ProjectResolver from the exact
// post-discovery catalog snapshot. Recovery deliberately does not use Project:
// stale or safety-degraded entries still need their durable restore journal
// resolved so Manager can either finish recovery or block that project.
func (c *Catalog) ResolveBackupProject(ctx context.Context, uid string) (backup.Project, error) {
	if err := ctx.Err(); err != nil {
		return backup.Project{}, err
	}
	c.mu.RLock()
	project, ok := c.projects[uid]
	c.mu.RUnlock()
	if !ok {
		return backup.Project{}, fmt.Errorf("%w: project %q is absent from the initial catalog", backup.ErrRecoveryRequired, uid)
	}
	return backup.Project{UID: project.UID, Name: project.Name, WorkingDir: project.WorkingDir}, nil
}

func cloneProjects(input map[string]Project) map[string]Project {
	result := make(map[string]Project, len(input))
	for uid, project := range input {
		result[uid] = cloneProject(project)
	}
	return result
}

func cloneProject(project Project) Project {
	project.Files = append([]projectmodel.FileFact(nil), project.Files...)
	project.ComposeFiles = append([]string(nil), project.ComposeFiles...)
	project.Services = append([]string(nil), project.Services...)
	project.EnvFiles = append([]EnvFileReference(nil), project.EnvFiles...)
	project.SourceReferences = append([]SourceReference(nil), project.SourceReferences...)
	project.ReadOnlyFiles = append([]safefile.ApprovedFile(nil), project.ReadOnlyFiles...)
	project.IncludedWorkDirs = append([]string(nil), project.IncludedWorkDirs...)
	return project
}
