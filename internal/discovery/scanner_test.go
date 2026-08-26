package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

type fakeSleeper struct {
	clock *fakeClock
	mu    sync.Mutex
	waits []time.Duration
}

func (s *fakeSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.waits = append(s.waits, duration)
	s.mu.Unlock()
	s.clock.Advance(duration)
	return nil
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func paths(files []File) []string {
	result := make([]string, len(files))
	for index, file := range files {
		result[index] = file.Path
	}
	return result
}

func TestScanFindsComposeFilesDeterministically(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{
		"z/docker-compose.yml",
		"a/compose.yaml",
		"a/compose.yml",
		".deploy/docker-compose.yaml",
		"not-compose.yaml",
		"nested/readme.txt",
	} {
		touch(t, filepath.Join(root, relative))
	}
	for _, ignored := range []string{".git", "node_modules", "vendor", ".cache", ".venv", "__pycache__", "target", "dist", "build"} {
		touch(t, filepath.Join(root, ignored, "compose.yaml"))
	}

	result, err := Scan(context.Background(), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, ".deploy", "docker-compose.yaml"),
		filepath.Join(root, "a", "compose.yaml"),
		filepath.Join(root, "z", "docker-compose.yml"),
	}
	if !reflect.DeepEqual(paths(result.Files), want) {
		t.Fatalf("files = %v, want %v", paths(result.Files), want)
	}
	if result.Truncated || result.StopReason != StopNone || result.LastScannedPath == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestScanTracksProjectDotEnvButNotStandaloneDotEnv(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "project", "compose.yaml"))
	touch(t, filepath.Join(root, "project", ".env"))
	touch(t, filepath.Join(root, "not-a-project", ".env"))

	result, err := Scan(context.Background(), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, "project", ".env"),
		filepath.Join(root, "project", "compose.yaml"),
	}
	if !reflect.DeepEqual(paths(result.Files), want) {
		t.Fatalf("files = %v, want %v", paths(result.Files), want)
	}
	if result.Files[0].Kind != FileKindEnv || result.Files[1].Kind != FileKindCompose {
		t.Fatalf("file kinds = %#v", result.Files)
	}
}

func TestScanProjectHashesOnlyTheRequestedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	requested := filepath.Join(root, "requested")
	other := filepath.Join(root, "other")
	touch(t, filepath.Join(requested, "compose.yaml"))
	touch(t, filepath.Join(requested, ".env"))
	touch(t, filepath.Join(other, "compose.yaml"))
	scanner, err := New(DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	files, err := scanner.ScanProject(context.Background(), root, requested)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(requested, ".env"), filepath.Join(requested, "compose.yaml")}
	if !reflect.DeepEqual(paths(files), want) {
		t.Fatalf("files = %v, want %v", paths(files), want)
	}
	if _, err := scanner.ScanProject(context.Background(), root, other+"-outside"); err == nil {
		t.Fatal("outside project refresh succeeded")
	}
}

func TestScanDoesNotFollowSymlinksOrEscapeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	touch(t, filepath.Join(root, "inside", "compose.yaml"))
	touch(t, filepath.Join(outside, "compose.yaml"))
	if err := os.Symlink(root, filepath.Join(root, "inside", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "compose.yaml"), filepath.Join(root, "docker-compose.yml")); err != nil {
		t.Fatal(err)
	}

	result, err := Scan(context.Background(), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "inside", "compose.yaml")}
	if !reflect.DeepEqual(paths(result.Files), want) {
		t.Fatalf("files = %v, want %v", paths(result.Files), want)
	}
	for _, file := range result.Files {
		if !withinRoot(root, file.Path) {
			t.Fatalf("file escaped root: %#v", file)
		}
	}
}

func TestPermissionDeniedReturnsVerifiedPartialResult(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a-readable", "compose.yaml"))
	blocked := filepath.Join(root, "z-blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("test process can read mode-000 directories")
	}

	result, err := Scan(context.Background(), DefaultConfig(root))
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("scan error=%v", err)
	}
	want := []string{filepath.Join(root, "a-readable", "compose.yaml")}
	if !reflect.DeepEqual(paths(result.Files), want) || !result.Truncated || result.StopReason != StopPermissionDenied ||
		result.LastScannedPath != blocked {
		t.Fatalf("result=%#v", result)
	}
}

func TestDirectoryBudgetReturnsDeterministicPartialResult(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "compose.yaml"))
	touch(t, filepath.Join(root, "a", "compose.yml"))
	touch(t, filepath.Join(root, "b", "docker-compose.yml"))
	config := DefaultConfig(root)
	config.MaxDirectories = 2 // root, then lexical child a
	result, err := Scan(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "a", "compose.yml"), filepath.Join(root, "compose.yaml")}
	if !reflect.DeepEqual(paths(result.Files), want) || !result.Truncated || result.StopReason != StopMaxDirectories || result.DirectoriesSeen != 2 || result.LastScannedPath != filepath.Join(root, "a") {
		t.Fatalf("result = %#v", result)
	}
}

func TestDurationBudgetStopsAtBoundary(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "compose.yaml"))
	touch(t, filepath.Join(root, "child", "compose.yaml"))
	clock := &fakeClock{now: time.Unix(1, 0)}
	advanced := false
	config := DefaultConfig(root)
	config.Clock = clock
	config.MaxDuration = time.Minute
	config.Ignore = func(entry IgnoreEntry) bool {
		if !advanced {
			advanced = true
			clock.Advance(time.Minute)
		}
		return false
	}
	result, err := Scan(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.StopReason != StopMaxDuration || result.DirectoriesSeen != 1 || result.LastScannedPath != root {
		t.Fatalf("result = %#v", result)
	}
}

func TestCustomIgnoreHookExcludesSubtree(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "keep", "compose.yaml"))
	touch(t, filepath.Join(root, "skip", "compose.yaml"))
	config := DefaultConfig(root)
	config.Ignore = func(entry IgnoreEntry) bool { return entry.Relative == "skip" }
	result, err := Scan(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "keep", "compose.yaml")}
	if !reflect.DeepEqual(paths(result.Files), want) {
		t.Fatalf("files = %v", paths(result.Files))
	}
}

func TestContextCancellationReturnsPartialResult(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a", "compose.yaml"))
	touch(t, filepath.Join(root, "b", "compose.yaml"))
	ctx, cancel := context.WithCancel(context.Background())
	config := DefaultConfig(root)
	config.Ignore = func(entry IgnoreEntry) bool {
		cancel()
		return false
	}
	result, err := Scan(ctx, config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if !result.Truncated || result.StopReason != StopContextCanceled || result.DirectoriesSeen != 1 || result.LastScannedPath != root {
		t.Fatalf("result = %#v", result)
	}
}

func TestCanonicalRootsAndDedup(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "compose.yaml"))
	alias := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(root, filepath.Join(root, "."), alias)
	result, err := Scan(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if result.DirectoriesSeen != 1 || len(result.Files) != 1 || result.Files[0].Root != root || result.Files[0].Path != filepath.Join(root, "compose.yaml") {
		t.Fatalf("result = %#v", result)
	}
}

func TestFileFactsIncludeSizeModTimeAndSHA256(t *testing.T) {
	root := t.TempDir()
	content := []byte("name: example\nservices: {}\n")
	filePath := filepath.Join(root, "compose.yaml")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(context.Background(), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 {
		t.Fatalf("files = %#v", result.Files)
	}
	wantHash := sha256.Sum256(content)
	file := result.Files[0]
	if file.Size != int64(len(content)) || file.ModTime.IsZero() || file.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("file facts = %#v", file)
	}
}

func TestSymlinkSwapAfterEntryInspectionIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(root, "compose.yaml")
	touch(t, target)
	outsideFile := filepath.Join(outside, "secret")
	if err := os.WriteFile(outsideFile, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped := false
	config := DefaultConfig(root)
	config.Ignore = func(entry IgnoreEntry) bool {
		if entry.Entry.Name() == "compose.yaml" && !swapped {
			swapped = true
			if err := os.Remove(target); err != nil {
				t.Fatalf("remove target: %v", err)
			}
			if err := os.Symlink(outsideFile, target); err != nil {
				t.Fatalf("swap target: %v", err)
			}
		}
		return false
	}
	result, err := Scan(context.Background(), config)
	if !HasScanErrorCode(err, CodeUnsafePath) || result.StopReason != StopUnsafePath || !result.Truncated || len(result.Files) != 0 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestNestedRootsAssignFileToMostSpecificRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "projects", "one")
	touch(t, filepath.Join(nested, "compose.yaml"))
	result, err := Scan(context.Background(), DefaultConfig(root, nested))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Root != nested {
		t.Fatalf("files = %#v", result.Files)
	}
}

func TestDirectoryRateLimiterUsesInjectedClockAndSleeper(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a", "compose.yaml"))
	touch(t, filepath.Join(root, "b", "compose.yaml"))
	clock := &fakeClock{now: time.Unix(1, 0)}
	sleeper := &fakeSleeper{clock: clock}
	config := DefaultConfig(root)
	config.Clock = clock
	config.Sleeper = sleeper
	config.DirectoriesPerSecond = 2
	result, err := Scan(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if result.DirectoriesSeen != 3 || result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	sleeper.mu.Lock()
	waits := append([]time.Duration(nil), sleeper.waits...)
	sleeper.mu.Unlock()
	if !reflect.DeepEqual(waits, []time.Duration{500 * time.Millisecond, 500 * time.Millisecond}) {
		t.Fatalf("rate waits = %v", waits)
	}
}

func TestComposeFilenameTable(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"compose.yaml", true},
		{"compose.yml", true},
		{"docker-compose.yaml", true},
		{"docker-compose.yml", true},
		{"compose.override.yaml", true},
		{"compose.override.yml", true},
		{"docker-compose.override.yaml", true},
		{"docker-compose.override.yml", true},
		{"Compose.yaml", false},
		{"docker-compose.json", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := composeFileNames[test.name]
			if got != test.want {
				t.Fatalf("compose name match = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSelectDefaultComposeFilesUsesComposePrecedence(t *testing.T) {
	root := "/srv/project"
	candidates := []File{
		{Path: filepath.Join(root, "docker-compose.yaml")},
		{Path: filepath.Join(root, "compose.yml")},
		{Path: filepath.Join(root, "compose.yaml")},
		{Path: filepath.Join(root, "docker-compose.override.yml")},
		{Path: filepath.Join(root, "compose.override.yaml")},
		{Path: filepath.Join(root, "compose.override.yml")},
	}
	selected := selectDefaultComposeFiles(candidates)
	if len(selected) != 2 || filepath.Base(selected[0].Path) != "compose.yaml" || filepath.Base(selected[1].Path) != "compose.override.yml" {
		t.Fatalf("selected files = %#v", selected)
	}
}

func TestScanIncludesDefaultOverrideAndIgnoresAlternativeBase(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"compose.yaml", "compose.yml", "compose.override.yaml"} {
		touch(t, filepath.Join(root, name))
	}
	result, err := Scan(context.Background(), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range result.Files {
		names = append(names, filepath.Base(file.Path))
	}
	if !reflect.DeepEqual(names, []string{"compose.override.yaml", "compose.yaml"}) {
		t.Fatalf("discovered files = %v", names)
	}
}
