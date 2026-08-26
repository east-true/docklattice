// Package discovery implements the filesystem half of Dockpilot Compose
// discovery. Docker label collection, project merging, and compose config
// evaluation intentionally live outside this package.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	productconfig "github.com/east-true/dockpilot/internal/config"
)

const (
	DefaultMaxDirectories = 200_000
	DefaultMaxDuration    = 60 * time.Second
)

var defaultIgnoredDirectories = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	".cache":       {},
	".venv":        {},
	"__pycache__":  {},
	"target":       {},
	"dist":         {},
	"build":        {},
}

// The order mirrors Docker Compose's default discovery contract. Dockpilot
// always passes explicit --file arguments after discovery, so it must select
// the same single base file and optional default override that Compose would
// have selected implicitly. Passing every filename found in a directory would
// merge alternatives that Compose normally treats as fallbacks.
var (
	defaultComposeFileNames = []string{
		"compose.yaml",
		"compose.yml",
		"docker-compose.yml",
		"docker-compose.yaml",
	}
	defaultComposeOverrideFileNames = []string{
		"compose.override.yml",
		"compose.override.yaml",
		"docker-compose.override.yml",
		"docker-compose.override.yaml",
	}
	composeFileNames = func() map[string]struct{} {
		result := make(map[string]struct{}, len(defaultComposeFileNames)+len(defaultComposeOverrideFileNames))
		for _, name := range append(append([]string(nil), defaultComposeFileNames...), defaultComposeOverrideFileNames...) {
			result[name] = struct{}{}
		}
		return result
	}()
)

// FileKind distinguishes the Compose configuration files supplied to the CLI
// from companion files that Compose consumes implicitly. Both kinds contribute
// to a project's discovery fingerprint.
type FileKind string

const (
	FileKindCompose FileKind = "COMPOSE"
	FileKindEnv     FileKind = "ENV"
)

// Clock makes the duration budget deterministic in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Sleeper makes optional I/O rate limiting cancelable and testable.
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// IgnoreEntry contains a root-relative filesystem candidate. Ignore hooks are
// called only after the built-in directory ignore check.
type IgnoreEntry struct {
	Root     string
	Path     string
	Relative string
	Entry    fs.DirEntry
}

// IgnoreFunc returns true to exclude a file or an entire directory subtree.
type IgnoreFunc func(IgnoreEntry) bool

type Config struct {
	Roots                []string
	MaxDirectories       int
	MaxDuration          time.Duration
	DirectoriesPerSecond int
	Ignore               IgnoreFunc
	Clock                Clock
	Sleeper              Sleeper
}

func DefaultConfig(roots ...string) Config {
	defaults := productconfig.V1Defaults()
	return Config{
		Roots:                append([]string(nil), roots...),
		MaxDirectories:       defaults.DiscoveryMaxDirectories,
		MaxDuration:          defaults.DiscoveryMaxDuration,
		DirectoriesPerSecond: defaults.DiscoveryDirectoriesPerSecond,
		Clock:                realClock{},
		Sleeper:              realSleeper{},
	}
}

type StopReason string

const (
	StopNone             StopReason = ""
	StopMaxDirectories   StopReason = "MAX_DIRECTORIES"
	StopMaxDuration      StopReason = "MAX_DURATION"
	StopContextCanceled  StopReason = "CONTEXT_CANCELED"
	StopPermissionDenied StopReason = "PERMISSION_DENIED"
	StopFilesystemError  StopReason = "FILESYSTEM_ERROR"
	StopUnsafePath       StopReason = "UNSAFE_PATH"
	StopFileUnstable     StopReason = "FILE_UNSTABLE"
)

type ScanErrorCode string

const (
	CodePermissionDenied ScanErrorCode = "PERMISSION_DENIED"
	CodeFilesystem       ScanErrorCode = "FILESYSTEM_ERROR"
	CodeUnsafePath       ScanErrorCode = "UNSAFE_PATH"
	CodeFileUnstable     ScanErrorCode = "FILE_UNSTABLE"
)

// ScanError identifies facts that cannot safely be accepted into discovery.
type ScanError struct {
	Code ScanErrorCode
	Path string
	Err  error
}

func (e *ScanError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Path, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Path)
}

func (e *ScanError) Unwrap() error { return e.Err }

func HasScanErrorCode(err error, code ScanErrorCode) bool {
	var target *ScanError
	return errors.As(err, &target) && target.Code == code
}

// File is one canonical, root-contained Compose file observation. SHA256 is
// lower-case hexadecimal over the exact descriptor contents whose metadata is
// represented by Size and ModTime.
type File struct {
	Root    string
	Path    string
	Kind    FileKind
	Size    int64
	ModTime time.Time
	SHA256  string
}

// Result remains useful when a budget, cancellation, or filesystem error
// stops the scan. LastScannedPath is the last directory whose entries were
// visited, not a speculative next path.
type Result struct {
	Files           []File
	DirectoriesSeen int
	Truncated       bool
	LastScannedPath string
	StopReason      StopReason
}

type Scanner struct {
	config Config
	roots  []string
}

var errDurationBudget = errors.New("discovery duration budget reached")

func New(config Config) (*Scanner, error) {
	if config.MaxDirectories == 0 {
		config.MaxDirectories = DefaultMaxDirectories
	}
	if config.MaxDuration == 0 {
		config.MaxDuration = DefaultMaxDuration
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Sleeper == nil {
		config.Sleeper = realSleeper{}
	}
	if config.MaxDirectories < 0 || config.MaxDuration < 0 || config.DirectoriesPerSecond < 0 {
		return nil, fmt.Errorf("discovery budgets must be positive")
	}
	if config.DirectoriesPerSecond > 0 && time.Second/time.Duration(config.DirectoriesPerSecond) <= 0 {
		return nil, fmt.Errorf("directories-per-second rate is too large")
	}

	unique := make(map[string]struct{}, len(config.Roots))
	roots := make([]string, 0, len(config.Roots))
	for _, configured := range config.Roots {
		if strings.IndexByte(configured, 0) >= 0 {
			return nil, fmt.Errorf("discovery root contains NUL")
		}
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return nil, fmt.Errorf("canonicalize discovery root %q: %w", configured, err)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve discovery root %q: %w", configured, err)
		}
		canonical = filepath.Clean(canonical)
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, fmt.Errorf("stat discovery root %q: %w", configured, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("discovery root %q is not a directory", configured)
		}
		if _, exists := unique[canonical]; exists {
			continue
		}
		unique[canonical] = struct{}{}
		roots = append(roots, canonical)
	}
	sort.Strings(roots)
	return &Scanner{config: config, roots: roots}, nil
}

// Scan is a convenience wrapper for constructing and running a Scanner.
func Scan(ctx context.Context, config Config) (Result, error) {
	scanner, err := New(config)
	if err != nil {
		return Result{}, err
	}
	return scanner.Scan(ctx)
}

type pendingDirectory struct {
	root string
	path string
}

func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	result := Result{}
	if len(s.roots) == 0 {
		return result, nil
	}
	started := s.config.Clock.Now()
	stack := make([]pendingDirectory, 0, len(s.roots))
	// Reverse push makes the lexically first root the first pop.
	for index := len(s.roots) - 1; index >= 0; index-- {
		stack = append(stack, pendingDirectory{root: s.roots[index], path: s.roots[index]})
	}
	found := make(map[string]File)
	var nextDirectoryAt time.Time
	if s.config.DirectoriesPerSecond > 0 {
		nextDirectoryAt = started
	}

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return finish(result, found, StopContextCanceled), err
		}
		if s.config.Clock.Now().Sub(started) >= s.config.MaxDuration {
			return finish(result, found, StopMaxDuration), nil
		}
		if result.DirectoriesSeen >= s.config.MaxDirectories {
			return finish(result, found, StopMaxDirectories), nil
		}
		if s.config.DirectoriesPerSecond > 0 {
			now := s.config.Clock.Now()
			if now.Before(nextDirectoryAt) {
				if err := s.config.Sleeper.Sleep(ctx, nextDirectoryAt.Sub(now)); err != nil {
					return finish(result, found, StopContextCanceled), err
				}
			}
			if err := ctx.Err(); err != nil {
				return finish(result, found, StopContextCanceled), err
			}
			now = s.config.Clock.Now()
			if now.Sub(started) >= s.config.MaxDuration {
				return finish(result, found, StopMaxDuration), nil
			}
			nextDirectoryAt = now.Add(time.Second / time.Duration(s.config.DirectoriesPerSecond))
		}

		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !withinRoot(current.root, current.path) {
			continue
		}
		result.DirectoriesSeen++
		result.LastScannedPath = current.path
		relativeDirectory, err := filepath.Rel(current.root, current.path)
		if err != nil {
			return finish(result, found, StopUnsafePath), &ScanError{Code: CodeUnsafePath, Path: current.path, Err: err}
		}
		entries, err := secureDirectoryEntries(current.root, relativeDirectory)
		if err != nil {
			return finish(result, found, stopReasonForError(err)), err
		}
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		if err := ctx.Err(); err != nil {
			return finish(result, found, StopContextCanceled), err
		}
		if s.config.Clock.Now().Sub(started) >= s.config.MaxDuration {
			return finish(result, found, StopMaxDuration), nil
		}

		children := make([]pendingDirectory, 0)
		composeFiles := make([]File, 0)
		envFiles := make([]File, 0, 1)
		for _, entry := range entries { // os.ReadDir is filename-sorted.
			if err := ctx.Err(); err != nil {
				return finish(result, found, StopContextCanceled), err
			}
			if s.config.Clock.Now().Sub(started) >= s.config.MaxDuration {
				return finish(result, found, StopMaxDuration), nil
			}
			entryPath := filepath.Clean(filepath.Join(current.path, entry.Name()))
			if !withinRoot(current.root, entryPath) {
				continue
			}
			relative, err := filepath.Rel(current.root, entryPath)
			if err != nil {
				continue
			}
			if entry.IsDir() {
				if _, ignored := defaultIgnoredDirectories[entry.Name()]; ignored {
					continue
				}
			}
			candidate := IgnoreEntry{Root: current.root, Path: entryPath, Relative: relative, Entry: entry}
			ignored := s.config.Ignore != nil && s.config.Ignore(candidate)
			if err := ctx.Err(); err != nil {
				return finish(result, found, StopContextCanceled), err
			}
			if s.config.Clock.Now().Sub(started) >= s.config.MaxDuration {
				return finish(result, found, StopMaxDuration), nil
			}
			if ignored {
				continue
			}
			// Never resolve an entry symlink. This prevents loops and files or
			// directories from importing paths outside the configured root.
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if entry.IsDir() {
				children = append(children, pendingDirectory{root: current.root, path: entryPath})
				continue
			}
			kind, tracked := trackedFileKind(entry.Name())
			if !tracked {
				continue
			}
			facts, err := secureFileFactsBudgeted(ctx, current.root, relative, nil, func() error {
				if s.config.Clock.Now().Sub(started) >= s.config.MaxDuration {
					return errDurationBudget
				}
				return nil
			})
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return finish(result, found, StopContextCanceled), err
				}
				if errors.Is(err, errDurationBudget) {
					return finish(result, found, StopMaxDuration), nil
				}
				return finish(result, found, stopReasonForError(err)), err
			}
			candidateFile := File{
				Root: current.root, Path: entryPath, Kind: kind,
				Size: facts.Size, ModTime: facts.ModTime, SHA256: facts.SHA256,
			}
			if kind == FileKindCompose {
				composeFiles = append(composeFiles, candidateFile)
			} else {
				envFiles = append(envFiles, candidateFile)
			}
		}
		// A .env without a local Compose configuration is not a project. Keep
		// companion files only when the directory was independently discovered
		// as a managed Compose project.
		composeFiles = selectDefaultComposeFiles(composeFiles)
		if len(composeFiles) > 0 {
			for _, candidateFile := range append(composeFiles, envFiles...) {
				if previous, exists := found[candidateFile.Path]; !exists || moreSpecificRoot(current.root, previous.Root) {
					found[candidateFile.Path] = candidateFile
				}
			}
		}
		// Reverse push preserves depth-first lexical traversal.
		for index := len(children) - 1; index >= 0; index-- {
			stack = append(stack, children[index])
		}
	}
	return finish(result, found, StopNone), nil
}

// ScanProject refreshes exactly one already-discovered project directory. It
// uses the same descriptor-relative, no-symlink observations as Scan, but
// deliberately never walks descendants or any other discovery root.
func (s *Scanner) ScanProject(ctx context.Context, root, workingDir string) ([]File, error) {
	root = filepath.Clean(root)
	workingDir = filepath.Clean(workingDir)
	if !s.hasRoot(root) || !withinRoot(root, workingDir) {
		return nil, &ScanError{Code: CodeUnsafePath, Path: workingDir, Err: errors.New("project is outside its configured discovery root")}
	}
	relative, err := filepath.Rel(root, workingDir)
	if err != nil {
		return nil, &ScanError{Code: CodeUnsafePath, Path: workingDir, Err: err}
	}
	entries, err := secureDirectoryEntries(root, relative)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	files := make([]File, 0, len(entries))
	composeCandidates := make([]File, 0, len(entries))
	envFiles := make([]File, 0, 1)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Clean(filepath.Join(workingDir, entry.Name()))
		if !withinRoot(root, path) {
			continue
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return nil, &ScanError{Code: CodeUnsafePath, Path: path, Err: err}
		}
		candidate := IgnoreEntry{Root: root, Path: path, Relative: relativePath, Entry: entry}
		if s.config.Ignore != nil && s.config.Ignore(candidate) {
			continue
		}
		kind, tracked := trackedFileKind(entry.Name())
		if !tracked {
			continue
		}
		facts, err := secureFileFacts(ctx, root, relativePath)
		if err != nil {
			return nil, err
		}
		candidateFile := File{Root: root, Path: path, Kind: kind, Size: facts.Size, ModTime: facts.ModTime, SHA256: facts.SHA256}
		if kind == FileKindCompose {
			composeCandidates = append(composeCandidates, candidateFile)
		} else {
			envFiles = append(envFiles, candidateFile)
		}
	}
	composeFiles := selectDefaultComposeFiles(composeCandidates)
	if len(composeFiles) == 0 {
		return nil, &ScanError{Code: CodeFileUnstable, Path: workingDir, Err: errors.New("project no longer has a Compose configuration file")}
	}
	files = append(files, composeFiles...)
	files = append(files, envFiles...)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return files, nil
}

func (s *Scanner) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if configured == root {
			return true
		}
	}
	return false
}

func trackedFileKind(name string) (FileKind, bool) {
	if _, compose := composeFileNames[name]; compose {
		return FileKindCompose, true
	}
	// Docker Compose reads a project-directory .env implicitly. It must be in
	// the fingerprint so name/service evaluation and Tier-1 drift are refreshed
	// when that file changes, but it is never a Compose --file input.
	if name == ".env" {
		return FileKindEnv, true
	}
	return "", false
}

func selectDefaultComposeFiles(candidates []File) []File {
	byName := make(map[string]File, len(candidates))
	for _, candidate := range candidates {
		byName[filepath.Base(candidate.Path)] = candidate
	}
	result := make([]File, 0, 2)
	for _, name := range defaultComposeFileNames {
		if candidate, found := byName[name]; found {
			result = append(result, candidate)
			break
		}
	}
	if len(result) == 0 {
		return nil
	}
	for _, name := range defaultComposeOverrideFileNames {
		if candidate, found := byName[name]; found {
			result = append(result, candidate)
			break
		}
	}
	return result
}

func stopReasonForError(err error) StopReason {
	if errors.Is(err, fs.ErrPermission) {
		return StopPermissionDenied
	}
	var scanError *ScanError
	if errors.As(err, &scanError) {
		switch scanError.Code {
		case CodePermissionDenied:
			return StopPermissionDenied
		case CodeUnsafePath:
			return StopUnsafePath
		case CodeFileUnstable:
			return StopFileUnstable
		}
	}
	return StopFilesystemError
}

func moreSpecificRoot(candidate, current string) bool {
	if candidate == current {
		return false
	}
	return withinRoot(current, candidate)
}

func finish(result Result, found map[string]File, reason StopReason) Result {
	result.StopReason = reason
	result.Truncated = reason != StopNone
	result.Files = make([]File, 0, len(found))
	for _, file := range found {
		result.Files = append(result.Files, file)
	}
	sort.Slice(result.Files, func(left, right int) bool {
		if result.Files[left].Path == result.Files[right].Path {
			return result.Files[left].Root < result.Files[right].Root
		}
		return result.Files[left].Path < result.Files[right].Path
	})
	return result
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
