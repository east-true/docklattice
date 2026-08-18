// Package projectmodel owns Server-side Compose project identity, merge, name
// collision, and Tier-1 drift decisions. Agents report raw filesystem and
// Docker facts; this package never parses Compose files or Docker config hashes.
package projectmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/east-true/dockpilot/internal/agentid"
)

var ErrInvalidFact = errors.New("invalid project discovery fact")

type Drift string

const (
	DriftInSync     Drift = "in-sync"
	DriftChanged    Drift = "changed"
	DriftNoBaseline Drift = "no-baseline"
)

type FileFact struct {
	Path   string
	Size   int64
	SHA256 string
}

// FilesystemProject is the output of safe discovery plus one delegated
// `docker compose config --format json` evaluation.
type FilesystemProject struct {
	WorkingDir       string
	Name             string
	Files            []FileFact
	Services         []string
	IncludedWorkDirs []string
}

// DockerFact is a raw observation from public Compose container labels. The
// config hash is carried for diagnostics only and never participates in drift.
type DockerFact struct {
	ContainerID string
	ProjectName string
	WorkingDir  string
	ConfigFiles []string
	Service     string
	ConfigHash  string
}

type Project struct {
	UID                 string
	AgentID             string
	WorkingDir          string
	Name                string
	Files               []FileFact
	Services            []string
	ContainerIDs        []string
	Managed             bool
	UnmanagedReason     string
	IncludedBy          []string
	NameCollision       bool
	MutationBlockReason string
	CurrentFingerprint  string
	AppliedFingerprint  string
	Drift               Drift
}

// UID returns the stable identity defined by architecture section 7.5. The
// NUL separator makes the concatenation unambiguous without changing either
// identity input.
func UID(agentID, canonicalWorkingDir string) (string, error) {
	if !agentid.Valid(agentID) {
		return "", fmt.Errorf("%w: invalid agent_id", ErrInvalidFact)
	}
	dir, err := validateCanonicalDir(canonicalWorkingDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(agentID + "\x00" + dir))
	return hex.EncodeToString(sum[:]), nil
}

// Fingerprint is a deterministic path+SHA set. Size and mtime are observations
// for change detection, not the Tier-1 applied fingerprint contract.
func Fingerprint(files []FileFact) (string, error) {
	normalized, err := normalizeFiles(files)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, file := range normalized {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.SHA256))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func DriftStatus(applied, current string) Drift {
	if applied == "" {
		return DriftNoBaseline
	}
	if applied == current {
		return DriftInSync
	}
	return DriftChanged
}

// ChangedFiles reports additions, removals, or content changes. Timestamp-only
// churn is not an external config change when the content hash is unchanged.
func ChangedFiles(previous, current []FileFact) ([]string, error) {
	left, err := fileHashes(previous)
	if err != nil {
		return nil, err
	}
	right, err := fileHashes(current)
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0)
	for path, oldHash := range left {
		if newHash, ok := right[path]; !ok || newHash != oldHash {
			changed = append(changed, path)
		}
	}
	for path := range right {
		if _, ok := left[path]; !ok {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// Merge normalizes raw facts for one Agent. applied is keyed by project UID.
// Filesystem identity wins; Docker labels only attach runtime observations.
func Merge(agentID string, filesystem []FilesystemProject, docker []DockerFact, applied map[string]string) ([]Project, error) {
	if !agentid.Valid(agentID) {
		return nil, fmt.Errorf("%w: invalid agent_id", ErrInvalidFact)
	}
	projects := make([]Project, 0, len(filesystem)+len(docker))
	byDir := make(map[string]int, len(filesystem))
	for _, fact := range filesystem {
		dir, err := validateCanonicalDir(fact.WorkingDir)
		if err != nil {
			return nil, err
		}
		if len(fact.Files) == 0 {
			return nil, fmt.Errorf("%w: filesystem project has no Compose files", ErrInvalidFact)
		}
		for _, includedDir := range fact.IncludedWorkDirs {
			if _, err := validateCanonicalDir(includedDir); err != nil {
				return nil, err
			}
		}
		if _, duplicate := byDir[dir]; duplicate {
			return nil, fmt.Errorf("%w: duplicate filesystem working_dir %q", ErrInvalidFact, dir)
		}
		files, err := normalizeFiles(fact.Files)
		if err != nil {
			return nil, err
		}
		uid, _ := UID(agentID, dir)
		fingerprint, _ := Fingerprint(files)
		projects = append(projects, Project{
			UID: uid, AgentID: agentID, WorkingDir: dir, Name: fact.Name,
			Files: files, Services: sortedUnique(fact.Services), Managed: true,
			CurrentFingerprint: fingerprint, AppliedFingerprint: applied[uid],
			Drift: DriftStatus(applied[uid], fingerprint),
		})
		byDir[dir] = len(projects) - 1
	}

	for _, fact := range docker {
		if fact.ProjectName == "" || fact.ContainerID == "" {
			return nil, fmt.Errorf("%w: Docker fact lacks project/container identity", ErrInvalidFact)
		}
		if fact.WorkingDir != "" {
			dir, err := validateCanonicalDir(fact.WorkingDir)
			if err != nil {
				return nil, err
			}
			if index, ok := byDir[dir]; ok {
				projects[index].ContainerIDs = append(projects[index].ContainerIDs, fact.ContainerID)
				projects[index].Services = append(projects[index].Services, fact.Service)
				continue
			}
			uid, _ := UID(agentID, dir)
			projects = append(projects, Project{
				UID: uid, AgentID: agentID, WorkingDir: dir, Name: fact.ProjectName,
				Services: sortedUnique([]string{fact.Service}), ContainerIDs: []string{fact.ContainerID},
				Managed: false, UnmanagedReason: "working_dir is outside or absent from discovery results",
				MutationBlockReason: "UNMANAGED_COMPOSE_PROJECT", Drift: DriftNoBaseline,
			})
			byDir[dir] = len(projects) - 1
			continue
		}
		// A missing working_dir has no architecture-defined stable project UID.
		// Preserve it as a first-class read-only observation without inventing one.
		projects = append(projects, Project{
			AgentID: agentID, Name: fact.ProjectName, Services: sortedUnique([]string{fact.Service}),
			ContainerIDs: []string{fact.ContainerID}, Managed: false,
			UnmanagedReason:     "Docker Compose working_dir label is missing",
			MutationBlockReason: "UNMANAGED_COMPOSE_PROJECT", Drift: DriftNoBaseline,
		})
	}

	for i := range projects {
		projects[i].Services = sortedUniqueNonEmpty(projects[i].Services)
		projects[i].ContainerIDs = sortedUniqueNonEmpty(projects[i].ContainerIDs)
	}
	linkIncludes(projects, filesystem, byDir)
	markNameCollisions(projects)
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].WorkingDir != projects[j].WorkingDir {
			return projects[i].WorkingDir < projects[j].WorkingDir
		}
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return strings.Join(projects[i].ContainerIDs, "\x00") < strings.Join(projects[j].ContainerIDs, "\x00")
	})
	return projects, nil
}

func linkIncludes(projects []Project, facts []FilesystemProject, byDir map[string]int) {
	for _, parent := range facts {
		parentIndex, ok := byDir[parent.WorkingDir]
		if !ok {
			continue
		}
		for _, childDir := range parent.IncludedWorkDirs {
			if childIndex, ok := byDir[filepath.Clean(childDir)]; ok && childIndex != parentIndex {
				projects[childIndex].IncludedBy = append(projects[childIndex].IncludedBy, projects[parentIndex].UID)
			}
		}
	}
	for i := range projects {
		projects[i].IncludedBy = sortedUniqueNonEmpty(projects[i].IncludedBy)
	}
}

func markNameCollisions(projects []Project) {
	byName := make(map[string][]int)
	for index := range projects {
		if projects[index].Name != "" && projects[index].WorkingDir != "" {
			byName[projects[index].Name] = append(byName[projects[index].Name], index)
		}
	}
	for _, indexes := range byName {
		dirs := make(map[string]struct{})
		for _, index := range indexes {
			dirs[projects[index].WorkingDir] = struct{}{}
		}
		if len(dirs) < 2 {
			continue
		}
		for _, index := range indexes {
			projects[index].NameCollision = true
			projects[index].MutationBlockReason = "PROJECT_NAME_COLLISION"
		}
	}
}

func normalizeFiles(files []FileFact) ([]FileFact, error) {
	result := append([]FileFact(nil), files...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		if !filepath.IsAbs(result[index].Path) || filepath.Clean(result[index].Path) != result[index].Path || result[index].Size < 0 {
			return nil, fmt.Errorf("%w: invalid file fact path/size", ErrInvalidFact)
		}
		hash, err := hex.DecodeString(result[index].SHA256)
		if err != nil || len(hash) != sha256.Size || strings.ToLower(result[index].SHA256) != result[index].SHA256 {
			return nil, fmt.Errorf("%w: invalid file SHA-256", ErrInvalidFact)
		}
		if _, duplicate := seen[result[index].Path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate file %q", ErrInvalidFact, result[index].Path)
		}
		seen[result[index].Path] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func fileHashes(files []FileFact) (map[string]string, error) {
	normalized, err := normalizeFiles(files)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(normalized))
	for _, file := range normalized {
		result[file.Path] = file.SHA256
	}
	return result, nil
}

func validateCanonicalDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || (dir != string(filepath.Separator) && strings.HasSuffix(dir, string(filepath.Separator))) {
		return "", fmt.Errorf("%w: working_dir must be canonical absolute path", ErrInvalidFact)
	}
	return dir, nil
}

func sortedUnique(values []string) []string { return sortedUniqueNonEmpty(values) }

func sortedUniqueNonEmpty(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
