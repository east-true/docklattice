//go:build linux

// Package composesource extracts only Compose source-file relationships.
//
// It deliberately does not evaluate variables, merge models, derive project
// identity, or select services. Docker Compose remains the sole evaluator for
// those semantics. This bounded reader exists because `docker compose config
// --format json` flattens include and extends provenance.
package composesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/east-true/docklattice/internal/safefile"
	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxFiles = 64
	DefaultMaxEdges = 256
	DefaultMaxDepth = 16
)

type Kind string

const (
	KindInclude Kind = "include"
	KindExtends Kind = "extends"
)

// Reference is source provenance only. Accessible says an fd-relative,
// no-symlink read succeeded. ReadOnly says the path is also inside workingDir
// and eligible for the catalog's temporary project-file approval.
type Reference struct {
	Kind       Kind
	Path       string
	Accessible bool
	ReadOnly   bool
}

// File is one safely observed Compose source or include environment input. It
// contains only path, size, and digest; environment contents never escape.
type File struct {
	Path   string
	Size   int64
	SHA256 string
}

// Result is intentionally incomplete rather than permissive when the narrow
// extractor cannot establish a complete bounded graph. Callers must then skip
// Compose evaluation caching; they may still delegate evaluation to Docker.
type Result struct {
	References       []Reference
	Files            []File
	ReadOnlyPaths    []string
	IncludedWorkDirs []string
	Complete         bool
}

type Analyzer struct {
	MaxFiles int
	MaxEdges int
	MaxDepth int
}

func New() Analyzer {
	return Analyzer{MaxFiles: DefaultMaxFiles, MaxEdges: DefaultMaxEdges, MaxDepth: DefaultMaxDepth}
}

// Analyze starts from already-discovered Compose files. It follows literal
// include and services.*.extends.file values anywhere under the verified
// discovery root so every safely resolvable Compose input can participate in
// drift detection. Only paths under the current project's working directory
// are exposed as read-only project-file approvals.
func (analyzer Analyzer) Analyze(ctx context.Context, discoveryRoot, workingDir string, files []string) (Result, error) {
	limits := analyzer.limits()
	if !canonicalDirectory(discoveryRoot) || !canonicalDirectory(workingDir) || !within(discoveryRoot, workingDir) {
		return Result{}, errors.New("composesource: discovery root and working directory must be canonical and nested")
	}
	if len(files) == 0 || len(files) > limits.maxFiles {
		return Result{}, errors.New("composesource: initial Compose file count is invalid")
	}

	queued := make([]sourceNode, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, path := range files {
		if !canonicalFile(path) || !within(workingDir, path) {
			return Result{}, errors.New("composesource: initial Compose file is outside working directory")
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		queued = append(queued, sourceNode{path: path})
	}
	sort.Slice(queued, func(left, right int) bool { return queued[left].path < queued[right].path })

	result := Result{Complete: true}
	references := make(map[referenceKey]*Reference)
	readOnlyPaths := make(map[string]struct{})
	sourceFiles := make(map[string]File)
	includedDirs := make(map[string]struct{})
	processed := make(map[string]struct{}, len(queued))
	edges := 0

	for len(queued) != 0 {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		node := queued[0]
		queued = queued[1:]
		if _, complete := processed[node.path]; complete {
			continue
		}
		if len(processed) == limits.maxFiles || len(sourceFiles) == limits.maxFiles || node.depth > limits.maxDepth {
			result.Complete = false
			continue
		}
		processed[node.path] = struct{}{}

		file, readErr := readProjectSource(ctx, discoveryRoot, node.path)
		if readErr != nil {
			result.Complete = false
			continue
		}
		sourceFiles[node.path] = File{Path: node.path, Size: int64(len(file.Content)), SHA256: file.SHA256}
		insideWorkingDir := within(workingDir, node.path)
		markObserved(references, node.path, insideWorkingDir)
		if node.referenced && insideWorkingDir {
			relative, relativeErr := filepath.Rel(workingDir, node.path)
			if relativeErr != nil || !safeRelative(relative) {
				clear(file.Content)
				return Result{}, errors.New("composesource: internal source path escaped working directory")
			}
			readOnlyPaths[filepath.ToSlash(relative)] = struct{}{}
		}
		rawReferences, parseErr := extract(file.Content)
		clear(file.Content)
		if parseErr != nil {
			result.Complete = false
			continue
		}
		for _, rawReference := range rawReferences {
			edges++
			if edges > limits.maxEdges {
				result.Complete = false
				break
			}
			candidate, resolved := resolve(node.path, rawReference.path)
			if !resolved || !within(discoveryRoot, candidate) {
				result.Complete = false
				continue
			}
			key := referenceKey{kind: rawReference.kind, path: candidate}
			reference := references[key]
			if reference == nil {
				reference = &Reference{Kind: rawReference.kind, Path: candidate}
				references[key] = reference
			}
			if _, alreadyRead := sourceFiles[candidate]; alreadyRead {
				reference.Accessible = true
				reference.ReadOnly = within(workingDir, candidate)
			}
			if rawReference.kind == KindInclude && rawReference.includeEnvironment {
				projectDirectory := filepath.Dir(candidate)
				if rawReference.projectDirectory != "" {
					var projectDirectoryResolved bool
					projectDirectory, projectDirectoryResolved = resolve(node.path, rawReference.projectDirectory)
					if !projectDirectoryResolved || !within(discoveryRoot, projectDirectory) {
						result.Complete = false
						continue
					}
				}
				includedDirs[projectDirectory] = struct{}{}
				envFiles := rawReference.envFiles
				if !rawReference.envFileExplicit {
					envFiles = []string{filepath.Join(projectDirectory, ".env")}
				}
				for _, envFile := range envFiles {
					envPath := envFile
					if !filepath.IsAbs(envPath) {
						envPath = filepath.Clean(filepath.Join(filepath.Dir(node.path), envPath))
					}
					if !within(discoveryRoot, envPath) {
						result.Complete = false
						continue
					}
					envInput, found, envErr := digestInput(ctx, discoveryRoot, envPath, !rawReference.envFileExplicit)
					if envErr != nil {
						result.Complete = false
						continue
					}
					if found {
						if _, exists := sourceFiles[envInput.Path]; !exists && len(sourceFiles) == limits.maxFiles {
							result.Complete = false
							continue
						}
						sourceFiles[envInput.Path] = envInput
					}
				}
			}
			if _, prior := processed[candidate]; prior {
				// A source edge back to an earlier file is valid syntax to parse but
				// cannot produce a complete acyclic provenance graph.
				result.Complete = false
				continue
			}
			if len(processed)+len(queued) >= limits.maxFiles || node.depth+1 > limits.maxDepth {
				result.Complete = false
				continue
			}
			queued = append(queued, sourceNode{path: candidate, depth: node.depth + 1, referenced: true})
		}
	}

	result.References = sortedReferences(references)
	result.Files = sortedFiles(sourceFiles)
	result.ReadOnlyPaths = sortedPaths(readOnlyPaths)
	result.IncludedWorkDirs = sortedDirectories(includedDirs)
	return result, nil
}

type limits struct {
	maxFiles int
	maxEdges int
	maxDepth int
}

func (analyzer Analyzer) limits() limits {
	result := limits{maxFiles: analyzer.MaxFiles, maxEdges: analyzer.MaxEdges, maxDepth: analyzer.MaxDepth}
	if result.maxFiles <= 0 {
		result.maxFiles = DefaultMaxFiles
	}
	if result.maxEdges <= 0 {
		result.maxEdges = DefaultMaxEdges
	}
	if result.maxDepth <= 0 {
		result.maxDepth = DefaultMaxDepth
	}
	return result
}

type sourceNode struct {
	path       string
	depth      int
	referenced bool
}

type referenceKey struct {
	kind Kind
	path string
}

type rawReference struct {
	kind               Kind
	path               string
	projectDirectory   string
	envFiles           []string
	envFileExplicit    bool
	includeEnvironment bool
}

func readProjectSource(ctx context.Context, discoveryRoot, path string) (safefile.File, error) {
	relative, err := filepath.Rel(discoveryRoot, path)
	if err != nil || !safeRelative(relative) {
		return safefile.File{}, errors.New("source path escapes discovery root")
	}
	root, err := safefile.OpenRoot(discoveryRoot, []safefile.ApprovedFile{{RelativePath: filepath.ToSlash(relative), Access: safefile.ReadOnly}})
	if err != nil {
		return safefile.File{}, err
	}
	defer root.Close()
	return root.Read(ctx, filepath.ToSlash(relative))
}

func digestInput(ctx context.Context, discoveryRoot, path string, optional bool) (File, bool, error) {
	relative, err := filepath.Rel(discoveryRoot, path)
	if err != nil || !safeRelative(relative) {
		return File{}, false, errors.New("input path escapes discovery root")
	}
	approved := []safefile.ApprovedFile{{RelativePath: filepath.ToSlash(relative), Access: safefile.ReadOnly}}
	root, err := safefile.OpenRoot(discoveryRoot, approved)
	if err != nil {
		return File{}, false, err
	}
	defer root.Close()
	digest, err := root.DigestReadOnly(ctx, filepath.ToSlash(relative))
	if err != nil {
		var pathErr *safefile.PathError
		if optional && errors.As(err, &pathErr) && errors.Is(pathErr.Err, os.ErrNotExist) {
			return File{}, false, nil
		}
		return File{}, false, err
	}
	return File{Path: path, Size: digest.Size, SHA256: digest.SHA256}, true, nil
}

func extract(content []byte) ([]rawReference, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode source YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple YAML documents are unsupported for source provenance")
		}
		return nil, fmt.Errorf("decode trailing source YAML: %w", err)
	}
	if err := rejectAliases(&document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("source YAML must be one document mapping")
	}
	root := document.Content[0]
	include, includeFound, err := uniqueMappingValue(root, "include")
	if err != nil {
		return nil, err
	}
	references := make([]rawReference, 0)
	if includeFound {
		includeRefs, includeErr := includeReferences(include)
		if includeErr != nil {
			return nil, includeErr
		}
		references = append(references, includeRefs...)
	}
	services, servicesFound, err := uniqueMappingValue(root, "services")
	if err != nil {
		return nil, err
	}
	if servicesFound {
		extends, extendsErr := extendsPaths(services)
		if extendsErr != nil {
			return nil, extendsErr
		}
		for _, path := range extends {
			references = append(references, rawReference{kind: KindExtends, path: path})
		}
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].kind != references[right].kind {
			return references[left].kind < references[right].kind
		}
		return references[left].path < references[right].path
	})
	return references, nil
}

func rejectAliases(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Tag == "!!merge" {
		return errors.New("YAML aliases, anchors, and merge keys are unsupported for source provenance")
	}
	for _, child := range node.Content {
		if err := rejectAliases(child); err != nil {
			return err
		}
	}
	return nil
}

func uniqueMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool, error) {
	if mapping.Kind != yaml.MappingNode || len(mapping.Content)%2 != 0 {
		return nil, false, errors.New("source YAML mapping is malformed")
	}
	var found *yaml.Node
	for index := 0; index < len(mapping.Content); index += 2 {
		mapKey, value := mapping.Content[index], mapping.Content[index+1]
		if mapKey.Kind != yaml.ScalarNode {
			return nil, false, errors.New("source YAML mapping key is not scalar")
		}
		if mapKey.Value != key {
			continue
		}
		if found != nil {
			return nil, false, fmt.Errorf("source YAML repeats %q", key)
		}
		found = value
	}
	return found, found != nil, nil
}

func includeReferences(node *yaml.Node) ([]rawReference, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		paths, err := validateLiteralPaths([]string{literalPath(node)})
		return rawIncludeReferences(paths, "", nil, false), err
	case yaml.SequenceNode:
		result := make([]rawReference, 0, len(node.Content))
		for _, entry := range node.Content {
			references, err := includeReferences(entry)
			if err != nil {
				return nil, err
			}
			result = append(result, references...)
		}
		return result, nil
	case yaml.MappingNode:
		path, found, err := uniqueMappingValue(node, "path")
		if err != nil || !found {
			return nil, errors.New("include map requires one literal path")
		}
		paths, pathErr := literalPathList(path)
		if pathErr != nil {
			return nil, pathErr
		}
		projectDirectory := ""
		if projectNode, projectFound, projectErr := uniqueMappingValue(node, "project_directory"); projectErr != nil {
			return nil, projectErr
		} else if projectFound {
			values, valueErr := literalPathList(projectNode)
			if valueErr != nil || len(values) != 1 {
				return nil, errors.New("include project_directory must be one literal path")
			}
			projectDirectory = values[0]
		}
		var envFiles []string
		envFileExplicit := false
		if envNode, envFound, envErr := uniqueMappingValue(node, "env_file"); envErr != nil {
			return nil, envErr
		} else if envFound {
			envFileExplicit = true
			envFiles, envErr = literalPathList(envNode)
			if envErr != nil {
				return nil, envErr
			}
		}
		return rawIncludeReferences(paths, projectDirectory, envFiles, envFileExplicit), nil
	default:
		return nil, errors.New("include value type is unsupported")
	}
}

func literalPathList(node *yaml.Node) ([]string, error) {
	values := make([]string, 0, 1)
	switch node.Kind {
	case yaml.ScalarNode:
		values = append(values, literalPath(node))
	case yaml.SequenceNode:
		for _, entry := range node.Content {
			if entry.Kind != yaml.ScalarNode {
				return nil, errors.New("include path list must contain literal paths")
			}
			values = append(values, literalPath(entry))
		}
	default:
		return nil, errors.New("include path must be a literal or list of literals")
	}
	return validateLiteralPaths(values)
}

func rawIncludeReferences(paths []string, projectDirectory string, envFiles []string, envFileExplicit bool) []rawReference {
	result := make([]rawReference, 0, len(paths))
	for index, path := range paths {
		result = append(result, rawReference{
			kind: KindInclude, path: path, projectDirectory: projectDirectory,
			envFiles: append([]string(nil), envFiles...), envFileExplicit: envFileExplicit, includeEnvironment: index == 0,
		})
	}
	return result
}

func extendsPaths(services *yaml.Node) ([]string, error) {
	if services.Kind != yaml.MappingNode || len(services.Content)%2 != 0 {
		return nil, errors.New("services source YAML must be a mapping")
	}
	result := make([]string, 0)
	for index := 0; index < len(services.Content); index += 2 {
		serviceName, service := services.Content[index], services.Content[index+1]
		if serviceName.Kind != yaml.ScalarNode || service.Kind != yaml.MappingNode {
			return nil, errors.New("service source YAML is unsupported")
		}
		extends, found, err := uniqueMappingValue(service, "extends")
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if extends.Kind != yaml.MappingNode {
			return nil, errors.New("service extends must be a mapping")
		}
		file, fileFound, fileErr := uniqueMappingValue(extends, "file")
		if fileErr != nil {
			return nil, fileErr
		}
		if !fileFound {
			continue
		}
		if file.Kind != yaml.ScalarNode {
			return nil, errors.New("service extends file must be a literal path")
		}
		result = append(result, literalPath(file))
	}
	return validateLiteralPaths(result)
}

func literalPath(node *yaml.Node) string {
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func validateLiteralPaths(paths []string) ([]string, error) {
	for _, path := range paths {
		if path == "" || strings.IndexByte(path, 0) >= 0 || strings.Contains(path, "$") {
			return nil, errors.New("source reference must be a non-empty literal path")
		}
	}
	return paths, nil
}

func resolve(parent, reference string) (string, bool) {
	if reference == "" || strings.IndexByte(reference, 0) >= 0 || strings.Contains(reference, "$") {
		return "", false
	}
	if filepath.IsAbs(reference) {
		return filepath.Clean(reference), true
	}
	return filepath.Clean(filepath.Join(filepath.Dir(parent), reference)), true
}

func canonicalDirectory(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func canonicalFile(path string) bool {
	return canonicalDirectory(filepath.Dir(path)) && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func safeRelative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator)) && !strings.ContainsRune(path, 0)
}

func markObserved(references map[referenceKey]*Reference, path string, readOnly bool) {
	for key, reference := range references {
		if key.path == path {
			reference.Accessible = true
			reference.ReadOnly = readOnly
		}
	}
}

func sortedReferences(references map[referenceKey]*Reference) []Reference {
	result := make([]Reference, 0, len(references))
	for _, reference := range references {
		result = append(result, *reference)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Kind != result[right].Kind {
			return result[left].Kind < result[right].Kind
		}
		return result[left].Path < result[right].Path
	})
	return result
}

func sortedFiles(files map[string]File) []File {
	result := make([]File, 0, len(files))
	for _, file := range files {
		result = append(result, file)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result
}

func sortedDirectories(directories map[string]struct{}) []string {
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result
}

func sortedPaths(paths map[string]struct{}) []string {
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}
