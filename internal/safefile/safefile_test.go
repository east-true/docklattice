//go:build linux

package safefile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadObservesContentHashModeAndLineEndings(t *testing.T) {
	t.Parallel()

	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("services:\r\n  app:\r\n"), 0o640)
	root := openTestRoot(t, dir, nil)

	file, err := root.Read(context.Background(), "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Content) != "services:\r\n  app:\r\n" {
		t.Fatalf("content = %q", file.Content)
	}
	if file.SHA256 != shaHex(file.Content) {
		t.Fatalf("sha256 = %q", file.SHA256)
	}
	if file.Mode.Perm() != 0o640 {
		t.Fatalf("mode = %04o", file.Mode.Perm())
	}
	if file.LineEndings != LineEndingsCRLF {
		t.Fatalf("line endings = %q", file.LineEndings)
	}
	file.Content[0] = 'X'
	again, err := root.Read(context.Background(), "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if again.Content[0] != 's' {
		t.Fatal("caller mutated stored/read content")
	}
}

func TestWriteValidatesSnapshotsAndAtomicallyPreservesModeAndCRLF(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("old:\r\n  value: 1\r\n"), 0o640)
	root := openTestRoot(t, dir, nil)
	original, err := root.Read(ctx, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}

	validated := false
	snapshotted := false
	result, err := root.Write(ctx, WriteRequest{
		RelativePath:   "compose.yaml",
		ExpectedSHA256: original.SHA256,
		Content:        []byte("new:\n  value: 2\n"),
		Validate: func(_ context.Context, input ValidationInput) error {
			validated = true
			if input.ProjectRoot != dir || input.RelativePath != "compose.yaml" {
				t.Fatalf("validation context = %+v", input)
			}
			if !strings.Contains(input.StagedRelativePath, ".docklattice-stage-") {
				t.Fatalf("staged relative path = %q", input.StagedRelativePath)
			}
			onDisk, err := os.ReadFile(input.StagedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(onDisk, input.StagedBytes) || string(onDisk) != "new:\r\n  value: 2\r\n" {
				t.Fatalf("staged bytes = %q / %q", onDisk, input.StagedBytes)
			}
			childRead, err := exec.Command("/bin/cat", input.StagedPath).Output()
			if err != nil {
				t.Fatalf("external validator cannot read staged path: %v", err)
			}
			if !bytes.Equal(childRead, input.StagedBytes) {
				t.Fatalf("external validator read = %q", childRead)
			}
			return nil
		},
		Snapshot: func(_ context.Context, input SnapshotInput) error {
			if !validated {
				t.Fatal("snapshot ran before validation")
			}
			snapshotted = true
			if input.Original.SHA256 != original.SHA256 || !bytes.Equal(input.Original.Content, original.Content) {
				t.Fatalf("snapshot original = %+v", input.Original)
			}
			return nil
		},
		Commit: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated || !snapshotted {
		t.Fatalf("callbacks: validated=%v snapshotted=%v", validated, snapshotted)
	}
	if string(result.Content) != "new:\r\n  value: 2\r\n" || result.LineEndings != LineEndingsCRLF {
		t.Fatalf("result = %+v", result)
	}
	if result.Mode.Perm() != 0o640 {
		t.Fatalf("result mode = %04o", result.Mode.Perm())
	}
	assertNoStages(t, dir)
}

func TestAllowlistDefaultsAndApprovedReferences(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, ".env.local", []byte("A=1\n"), 0o600)
	writeFixture(t, dir, "notes.txt", []byte("notes\n"), 0o600)
	if err := os.Mkdir(filepath.Join(dir, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "configs/referenced.env", []byte("B=2\n"), 0o600)
	root := openTestRoot(t, dir, []ApprovedFile{
		{RelativePath: "configs/referenced.env", Access: ReadOnly},
	})

	if _, err := root.Read(ctx, ".env.local"); err != nil {
		t.Fatalf("default env allowlist: %v", err)
	}
	if _, err := root.Read(ctx, "configs/referenced.env"); err != nil {
		t.Fatalf("approved reference: %v", err)
	}
	if _, err := root.Read(ctx, "notes.txt"); !errors.Is(err, ErrPath) {
		t.Fatalf("unapproved read error = %v", err)
	}
	ref, err := root.Read(ctx, "configs/referenced.env")
	if err != nil {
		t.Fatal(err)
	}
	_, err = root.Write(ctx, validWrite("configs/referenced.env", ref.SHA256, []byte("B=3\n")))
	if !errors.Is(err, ErrPath) {
		t.Fatalf("read-only reference write error = %v", err)
	}
}

func TestApprovedReferenceCannotDowngradeManagedDefaultFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("services: {}\n"), 0o600)
	root := openTestRoot(t, dir, []ApprovedFile{{RelativePath: "compose.yaml", Access: ReadOnly}})

	read, err := root.Read(ctx, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Write(ctx, WriteRequest{
		RelativePath: "compose.yaml", ExpectedSHA256: read.SHA256, Content: []byte("services: {web: {}}\n"),
		Validate: func(context.Context, ValidationInput) error { return nil },
		Snapshot: func(context.Context, SnapshotInput) error { return nil },
		Commit:   func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("managed default was downgraded by read-only approval: %v", err)
	}
}

func TestVerifyReadOnlyDoesNotReadContentsOrFollowSymlinks(t *testing.T) {
	t.Parallel()

	dir := projectDir(t)
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "config/service.env", []byte("TOP_SECRET=not-materialized\n"), 0o600)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "config", "linked.env")); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, dir, []ApprovedFile{
		{RelativePath: "config/service.env", Access: ReadOnly},
		{RelativePath: "config/linked.env", Access: ReadOnly},
	})
	if err := root.VerifyReadOnly(context.Background(), "config/service.env"); err != nil {
		t.Fatalf("verify safe reference: %v", err)
	}
	if err := root.VerifyReadOnly(context.Background(), "config/linked.env"); !errors.Is(err, ErrPath) {
		t.Fatalf("symlink verification error = %v", err)
	}
}

func TestDigestReadOnlyReturnsOnlyStableMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "config/service.env", []byte("SECRET=value\n"), 0o600)
	root, err := OpenRoot(dir, []ApprovedFile{{RelativePath: "config/service.env", Access: ReadOnly}})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	digest, err := root.DigestReadOnly(context.Background(), "config/service.env")
	if err != nil {
		t.Fatal(err)
	}
	if digest.RelativePath != "config/service.env" || digest.Size != int64(len("SECRET=value\n")) || digest.SHA256 != shaHex([]byte("SECRET=value\n")) {
		t.Fatalf("digest = %+v", digest)
	}
}

func TestTraversalAbsoluteNULAndSymlinksAreRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("safe\n"), 0o600)
	root := openTestRoot(t, dir, []ApprovedFile{
		{RelativePath: "sub/config.env", Access: ReadWrite},
		{RelativePath: "linked.env", Access: ReadWrite},
	})
	for _, path := range []string{"../compose.yaml", "sub/../../compose.yaml", "/etc/passwd", "compose.yaml\x00tail"} {
		if _, err := root.Read(ctx, path); !errors.Is(err, ErrPath) {
			t.Errorf("Read(%q) error = %v", path, err)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.env")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Read(ctx, "linked.env"); !errors.Is(err, ErrPath) {
		t.Fatalf("target symlink error = %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Read(ctx, "sub/config.env"); !errors.Is(err, ErrPath) {
		t.Fatalf("component symlink error = %v", err)
	}
}

func TestConcurrentEditReturnsCurrentContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("version: old\n"), 0o600)
	root := openTestRoot(t, dir, nil)
	read, err := root.Read(ctx, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("version: external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = root.Write(ctx, validWrite("compose.yaml", read.SHA256, []byte("version: ui\n")))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current == nil {
		t.Fatalf("write error = %v", err)
	}
	if string(conflict.Current.Content) != "version: external\n" {
		t.Fatalf("conflict current content = %q", conflict.Current.Content)
	}
}

func TestConcurrentWritesWithSameExpectedHashCommitOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("original\n"), 0o600)
	root := openTestRoot(t, dir, nil)
	read, err := root.Read(ctx, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errs := make(chan error, 2)
	for _, content := range []string{"first\n", "second\n"} {
		wait.Add(1)
		go func(content string) {
			defer wait.Done()
			_, err := root.Write(ctx, validWrite("compose.yaml", read.SHA256, []byte(content)))
			errs <- err
		}(content)
	}
	wait.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected write error: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestValidationSnapshotAndStageMutationFailuresKeepOriginal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*WriteRequest)
		want      error
	}{
		{
			name: "validation",
			configure: func(request *WriteRequest) {
				request.Validate = func(context.Context, ValidationInput) error { return errors.New("invalid compose") }
			},
			want: ErrValidation,
		},
		{
			name: "snapshot",
			configure: func(request *WriteRequest) {
				request.Snapshot = func(context.Context, SnapshotInput) error { return errors.New("snapshot unavailable") }
			},
			want: ErrSnapshot,
		},
		{
			name: "validator mutates stage",
			configure: func(request *WriteRequest) {
				request.Validate = func(_ context.Context, input ValidationInput) error {
					return os.WriteFile(input.StagedPath, []byte("mutated\n"), 0o600)
				}
			},
			want: ErrConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := projectDir(t)
			original := []byte("original\n")
			writeFixture(t, dir, "compose.yaml", original, 0o600)
			root := openTestRoot(t, dir, nil)
			read, err := root.Read(context.Background(), "compose.yaml")
			if err != nil {
				t.Fatal(err)
			}
			request := validWrite("compose.yaml", read.SHA256, []byte("new\n"))
			test.configure(&request)
			if _, err := root.Write(context.Background(), request); !errors.Is(err, test.want) {
				t.Fatalf("Write error = %v, want %v", err, test.want)
			}
			assertFileBytes(t, filepath.Join(dir, "compose.yaml"), original)
			assertNoStages(t, dir)
		})
	}
}

func TestTargetAndComponentSwapDuringWriteAreRejected(t *testing.T) {
	t.Parallel()

	t.Run("target regular swap before final check", func(t *testing.T) {
		dir := projectDir(t)
		path := filepath.Join(dir, "compose.yaml")
		writeFixture(t, dir, "compose.yaml", []byte("original\n"), 0o600)
		root, err := openRootWithHooks(dir, nil, faultHooks{beforeFinalCheck: func() error {
			return os.WriteFile(path, []byte("external\n"), 0o600)
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		read, _ := root.Read(context.Background(), "compose.yaml")
		_, err = root.Write(context.Background(), validWrite("compose.yaml", read.SHA256, []byte("new\n")))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("Write error = %v", err)
		}
		assertFileBytes(t, path, []byte("external\n"))
		assertNoStages(t, dir)
	})

	t.Run("target symlink swap at rename", func(t *testing.T) {
		dir := projectDir(t)
		path := filepath.Join(dir, "compose.yaml")
		moved := filepath.Join(dir, "moved-original")
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, "compose.yaml", []byte("original\n"), 0o600)
		root, err := openRootWithHooks(dir, nil, faultHooks{beforeRename: func() error {
			if err := os.Rename(path, moved); err != nil {
				return err
			}
			return os.Symlink(outside, path)
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		read, _ := root.Read(context.Background(), "compose.yaml")
		_, err = root.Write(context.Background(), validWrite("compose.yaml", read.SHA256, []byte("new\n")))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("Write error = %v", err)
		}
		outsideBytes, _ := os.ReadFile(outside)
		if string(outsideBytes) != "outside\n" {
			t.Fatalf("outside target changed: %q", outsideBytes)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("swapped symlink was not restored: info=%v err=%v", info, err)
		}
		assertFileBytes(t, moved, []byte("original\n"))
		assertNoStages(t, dir)
	})

	t.Run("directory component swap", func(t *testing.T) {
		dir := projectDir(t)
		if err := os.Mkdir(filepath.Join(dir, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, "config/override.yaml", []byte("original\n"), 0o600)
		moved := filepath.Join(dir, "config-moved")
		root, err := openRootWithHooks(dir, []ApprovedFile{{"config/override.yaml", ReadWrite}}, faultHooks{
			beforeFinalCheck: func() error {
				if err := os.Rename(filepath.Join(dir, "config"), moved); err != nil {
					return err
				}
				return os.Mkdir(filepath.Join(dir, "config"), 0o700)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		read, _ := root.Read(context.Background(), "config/override.yaml")
		_, err = root.Write(context.Background(), validWrite("config/override.yaml", read.SHA256, []byte("new\n")))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("Write error = %v", err)
		}
		assertFileBytes(t, filepath.Join(moved, "override.yaml"), []byte("original\n"))
		assertNoStages(t, moved)
	})
}

func TestInjectedWriteFaultsKeepOriginalAndCleanStage(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected")
	tests := []struct {
		name  string
		hooks faultHooks
	}{
		{name: "after staged sync", hooks: faultHooks{afterStageSync: func() error { return fault }}},
		{name: "before rename", hooks: faultHooks{beforeRename: func() error { return fault }}},
		{name: "before directory sync", hooks: faultHooks{beforeDirSync: func() error { return fault }}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := projectDir(t)
			original := []byte("original\n")
			writeFixture(t, dir, "compose.yaml", original, 0o600)
			root, err := openRootWithHooks(dir, nil, test.hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			read, _ := root.Read(context.Background(), "compose.yaml")
			_, err = root.Write(context.Background(), validWrite("compose.yaml", read.SHA256, []byte("new\n")))
			if !errors.Is(err, fault) {
				t.Fatalf("Write error = %v", err)
			}
			assertFileBytes(t, filepath.Join(dir, "compose.yaml"), original)
			assertNoStages(t, dir)
		})
	}
}

func TestSizeUTF8AndRequiredCallbacksAreTyped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	t.Run("oversized read", func(t *testing.T) {
		dir := projectDir(t)
		writeFixture(t, dir, "compose.yaml", bytes.Repeat([]byte{'x'}, int(MaxFileSize)+1), 0o600)
		root := openTestRoot(t, dir, nil)
		if _, err := root.Read(ctx, "compose.yaml"); !errors.Is(err, ErrSize) {
			t.Fatalf("Read error = %v", err)
		}
	})

	t.Run("invalid UTF-8 write", func(t *testing.T) {
		dir := projectDir(t)
		writeFixture(t, dir, "compose.yaml", []byte("old\n"), 0o600)
		root := openTestRoot(t, dir, nil)
		read, _ := root.Read(ctx, "compose.yaml")
		request := validWrite("compose.yaml", read.SHA256, []byte{0xff})
		if _, err := root.Write(ctx, request); !errors.Is(err, ErrValidation) {
			t.Fatalf("Write error = %v", err)
		}
	})

	t.Run("missing expected hash", func(t *testing.T) {
		dir := projectDir(t)
		writeFixture(t, dir, "compose.yaml", []byte("old\n"), 0o600)
		root := openTestRoot(t, dir, nil)
		request := validWrite("compose.yaml", "", []byte("new\n"))
		if _, err := root.Write(ctx, request); !errors.Is(err, ErrConflict) {
			t.Fatalf("Write error = %v", err)
		}
	})

	t.Run("missing validation", func(t *testing.T) {
		dir := projectDir(t)
		writeFixture(t, dir, "compose.yaml", []byte("old\n"), 0o600)
		root := openTestRoot(t, dir, nil)
		read, _ := root.Read(ctx, "compose.yaml")
		request := validWrite("compose.yaml", read.SHA256, []byte("new\n"))
		request.Validate = nil
		if _, err := root.Write(ctx, request); !errors.Is(err, ErrValidation) {
			t.Fatalf("Write error = %v", err)
		}
	})

	t.Run("missing commit gate", func(t *testing.T) {
		dir := projectDir(t)
		writeFixture(t, dir, "compose.yaml", []byte("old\n"), 0o600)
		root := openTestRoot(t, dir, nil)
		read, _ := root.Read(ctx, "compose.yaml")
		request := validWrite("compose.yaml", read.SHA256, []byte("new\n"))
		request.Commit = nil
		if _, err := root.Write(ctx, request); !errors.Is(err, ErrValidation) {
			t.Fatalf("Write error = %v", err)
		}
	})
}

func TestWriteCommitGateFailureLeavesOriginalAndRemovesStage(t *testing.T) {
	dir := projectDir(t)
	writeFixture(t, dir, "compose.yaml", []byte("old\n"), 0o600)
	root := openTestRoot(t, dir, nil)
	original, err := root.Read(context.Background(), "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	gateErr := errors.New("canceled before commit")
	request := validWrite("compose.yaml", original.SHA256, []byte("new\n"))
	request.Commit = func(context.Context) error { return gateErr }
	if _, err := root.Write(context.Background(), request); !errors.Is(err, gateErr) {
		t.Fatalf("Write error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old\n" {
		t.Fatalf("target after gate failure = %q", got)
	}
	assertNoStages(t, dir)
}

func validWrite(path, expected string, content []byte) WriteRequest {
	return WriteRequest{
		RelativePath: path, ExpectedSHA256: expected, Content: content,
		Validate: func(context.Context, ValidationInput) error { return nil },
		Snapshot: func(context.Context, SnapshotInput) error { return nil },
		Commit:   func(context.Context) error { return nil },
	}
}

func projectDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openTestRoot(t *testing.T, dir string, approved []ApprovedFile) *Root {
	t.Helper()
	root, err := OpenRoot(dir, approved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func writeFixture(t *testing.T, dir, relative string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertNoStages(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".docklattice-stage-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staged files remain: %v", matches)
	}
}
