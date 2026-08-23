//go:build linux

package safefile

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type openedFile struct {
	file   File
	stat   unix.Stat_t
	parent int
	name   string
	dirs   []string
}

// Read returns bounded UTF-8 content and observations without following any
// symlink component or target.
func (r *Root) Read(ctx context.Context, relativePath string) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return File{}, ErrClosed
	}
	path, err := r.authorize(relativePath, false)
	if err != nil {
		return File{}, err
	}
	opened, err := r.openExisting(ctx, path)
	if err != nil {
		return File{}, err
	}
	defer unix.Close(opened.parent)
	return cloneFile(opened.file), nil
}

// DigestReadOnly hashes one allowlisted regular file without retaining or
// returning its contents. The same descriptor-relative, no-symlink and stable
// stat checks used by Read protect the observation.
func (r *Root) DigestReadOnly(ctx context.Context, relativePath string) (Digest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Digest{}, ErrClosed
	}
	path, err := r.authorize(relativePath, false)
	if err != nil {
		return Digest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Digest{}, err
	}
	parent, _, err := r.walkParent(path)
	if err != nil {
		return Digest{}, err
	}
	defer unix.Close(parent)
	name := filepath.Base(path)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Digest{}, &PathError{Path: path, Reason: "open regular file without following symlinks", Err: err}
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return Digest{}, &PathError{Path: path, Reason: "stat opened file", Err: err}
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return Digest{}, &PathError{Path: path, Reason: "target is not a regular file"}
	}
	if before.Size > MaxFileSize {
		return Digest{}, &SizeError{Path: path, Size: before.Size, Limit: MaxFileSize}
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return Digest{}, fmt.Errorf("safefile: hash %q: %w", path, err)
	}
	if size > MaxFileSize {
		return Digest{}, &SizeError{Path: path, Size: size, Limit: MaxFileSize}
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return Digest{}, &PathError{Path: path, Reason: "stat opened file after hash", Err: err}
	}
	if !sameStableStat(before, after) {
		return Digest{}, &ConflictError{Path: path, Reason: "file changed while it was being hashed"}
	}
	return Digest{RelativePath: path, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// Write atomically replaces exactly one allowlisted existing file.
func (r *Root) Write(ctx context.Context, request WriteRequest) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stageMu.Lock()
	defer r.stageMu.Unlock()
	if r.closed {
		return File{}, ErrClosed
	}
	path, err := r.authorize(request.RelativePath, true)
	if err != nil {
		return File{}, err
	}
	if request.ExpectedSHA256 == "" {
		return File{}, &ConflictError{Path: path, Reason: "expected_sha256 is required"}
	}
	if request.Validate == nil {
		return File{}, &ValidationError{Path: path, Err: errors.New("validation callback is required")}
	}
	if request.Snapshot == nil {
		return File{}, &SnapshotError{Path: path, Err: errors.New("snapshot callback is required")}
	}
	if request.Commit == nil {
		return File{}, &ValidationError{Path: path, Err: errors.New("commit gate is required")}
	}
	if int64(len(request.Content)) > MaxFileSize {
		return File{}, &SizeError{Path: path, Size: int64(len(request.Content)), Limit: MaxFileSize}
	}
	if !utf8.Valid(request.Content) {
		return File{}, &ValidationError{Path: path, Err: errors.New("content is not valid UTF-8")}
	}

	original, err := r.openExisting(ctx, path)
	if err != nil {
		return File{}, err
	}
	defer unix.Close(original.parent)
	if request.ExpectedSHA256 != original.file.SHA256 {
		current := cloneFile(original.file)
		return File{}, &ConflictError{
			Path: path, ExpectedSHA256: request.ExpectedSHA256, Current: &current,
			Reason: "file changed since it was read",
		}
	}

	stagedBytes := preserveLineEndings(request.Content, original.file.LineEndings)
	if int64(len(stagedBytes)) > MaxFileSize {
		return File{}, &SizeError{Path: path, Size: int64(len(stagedBytes)), Limit: MaxFileSize}
	}
	tempName, tempFD, tempStat, err := createStaged(original.parent, original.stat, stagedBytes)
	if err != nil {
		return File{}, err
	}
	stagedExists := true
	defer func() {
		unix.Close(tempFD)
		if stagedExists {
			_ = unix.Unlinkat(original.parent, tempName, 0)
		}
	}()

	if r.hooks.afterStageSync != nil {
		if err := r.hooks.afterStageSync(); err != nil {
			return File{}, fmt.Errorf("safefile: injected staged sync failure: %w", err)
		}
	}
	validationBytes := append([]byte(nil), stagedBytes...)
	stagedRelative := strings.Join(append(original.dirs, tempName), "/")
	validation := ValidationInput{
		ProjectRoot: r.path, RelativePath: path, StagedRelativePath: stagedRelative,
		// A validator commonly execs the bundled Compose process. /proc/self
		// would then refer to that child, whose close-on-exec FD table does not
		// contain original.parent. Anchor the path to the still-running Agent.
		StagedPath:  fmt.Sprintf("/proc/%d/fd/%d/%s", os.Getpid(), original.parent, tempName),
		StagedBytes: validationBytes,
	}
	if err := request.Validate(ctx, validation); err != nil {
		return File{}, &ValidationError{Path: path, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	if err := verifyNamedFile(original.parent, tempName, tempStat, shaHex(stagedBytes)); err != nil {
		return File{}, &ConflictError{Path: path, ExpectedSHA256: shaHex(stagedBytes), Reason: "staged file changed during validation"}
	}
	if err := request.Snapshot(ctx, SnapshotInput{
		ProjectRoot: r.path, RelativePath: path, Original: cloneFile(original.file),
	}); err != nil {
		return File{}, &SnapshotError{Path: path, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	if r.hooks.beforeFinalCheck != nil {
		if err := r.hooks.beforeFinalCheck(); err != nil {
			return File{}, fmt.Errorf("safefile: injected final-check failure: %w", err)
		}
	}

	// Re-walk from the root after validation and snapshot. This detects a
	// swapped directory component; the inode comparison detects a replaced or
	// in-place edited target.
	currentParent, _, err := r.walkParent(path)
	if err != nil {
		return File{}, err
	}
	defer unix.Close(currentParent)
	if !sameFD(currentParent, original.parent) {
		return File{}, &ConflictError{Path: path, ExpectedSHA256: request.ExpectedSHA256, Reason: "parent directory changed"}
	}
	current, err := r.readAt(ctx, currentParent, original.name, path)
	if err != nil {
		return File{}, err
	}
	if !sameStableStat(current.stat, original.stat) || current.file.SHA256 != original.file.SHA256 {
		value := cloneFile(current.file)
		return File{}, &ConflictError{
			Path: path, ExpectedSHA256: request.ExpectedSHA256, Current: &value,
			Reason: "file changed during validation or snapshot",
		}
	}
	if err := verifyNamedFile(original.parent, tempName, tempStat, shaHex(stagedBytes)); err != nil {
		return File{}, &ConflictError{Path: path, ExpectedSHA256: shaHex(stagedBytes), Reason: "staged file changed before commit"}
	}
	if err := request.Commit(ctx); err != nil {
		return File{}, err
	}
	if r.hooks.beforeRename != nil {
		if err := r.hooks.beforeRename(); err != nil {
			return File{}, fmt.Errorf("safefile: injected rename failure: %w", err)
		}
	}

	// Exchange retains the displaced original at tempName until the directory
	// fsync succeeds. It also lets us inspect and roll back a last-instant target
	// swap instead of silently overwriting it.
	if err := unix.Renameat2(original.parent, tempName, original.parent, original.name, unix.RENAME_EXCHANGE); err != nil {
		return File{}, fmt.Errorf("safefile: atomic rename: %w", err)
	}
	rollback := func() error {
		return unix.Renameat2(original.parent, tempName, original.parent, original.name, unix.RENAME_EXCHANGE)
	}
	// renameat2 may update the displaced inode's ctime, so the post-exchange
	// check keeps identity, ownership, mode, size and content invariants while
	// deliberately ignoring timestamps.
	displacedOK := verifyRenamedFile(original.parent, tempName, original.stat, original.file.SHA256) == nil
	parentStillBound := r.parentStillBound(path, original.parent)
	if !displacedOK || !parentStillBound {
		rollbackErr := rollback()
		_ = syncDir(original.parent)
		if rollbackErr != nil {
			return File{}, fmt.Errorf("safefile: target swap detected and rollback failed: %w", rollbackErr)
		}
		return File{}, &ConflictError{Path: path, ExpectedSHA256: request.ExpectedSHA256, Reason: "target or parent swapped at commit"}
	}
	if r.hooks.beforeDirSync != nil {
		if err := r.hooks.beforeDirSync(); err != nil {
			rollbackErr := rollback()
			_ = syncDir(original.parent)
			if rollbackErr != nil {
				return File{}, fmt.Errorf("safefile: directory sync fault and rollback failed: %w", rollbackErr)
			}
			return File{}, fmt.Errorf("safefile: injected directory sync failure: %w", err)
		}
	}
	if err := syncDir(original.parent); err != nil {
		rollbackErr := rollback()
		_ = syncDir(original.parent)
		if rollbackErr != nil {
			return File{}, fmt.Errorf("safefile: directory sync failed (%v) and rollback failed: %w", err, rollbackErr)
		}
		return File{}, fmt.Errorf("safefile: sync directory: %w", err)
	}
	// The new target is durable. Remove the displaced original and sync the
	// cleanup; a cleanup sync failure does not invalidate the committed target.
	if err := unix.Unlinkat(original.parent, tempName, 0); err != nil {
		return File{}, fmt.Errorf("safefile: remove displaced original: %w", err)
	}
	stagedExists = false
	if err := syncDir(original.parent); err != nil {
		return File{}, fmt.Errorf("safefile: sync displaced-original cleanup: %w", err)
	}

	result, err := r.readAt(ctx, original.parent, original.name, path)
	if err != nil {
		return File{}, err
	}
	return cloneFile(result.file), nil
}

func (r *Root) openExisting(ctx context.Context, path string) (openedFile, error) {
	parent, dirs, err := r.walkParent(path)
	if err != nil {
		return openedFile{}, err
	}
	name := filepath.Base(path)
	opened, err := r.readAt(ctx, parent, name, path)
	if err != nil {
		unix.Close(parent)
		return openedFile{}, err
	}
	opened.parent = parent
	opened.name = name
	opened.dirs = dirs
	return opened, nil
}

func (r *Root) walkParent(path string) (int, []string, error) {
	components := strings.Split(path, "/")
	dirs := components[:len(components)-1]
	fd, err := unix.Dup(r.rootFD)
	if err != nil {
		return -1, nil, fmt.Errorf("safefile: duplicate project root fd: %w", err)
	}
	for _, component := range dirs {
		next, err := unix.Openat(fd, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return -1, nil, &PathError{Path: path, Reason: "symlink or invalid directory component", Err: err}
		}
		fd = next
	}
	return fd, append([]string(nil), dirs...), nil
}

func (r *Root) readAt(ctx context.Context, parent int, name, path string) (openedFile, error) {
	if err := ctx.Err(); err != nil {
		return openedFile{}, err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return openedFile{}, &PathError{Path: path, Reason: "open regular file without following symlinks", Err: err}
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return openedFile{}, &PathError{Path: path, Reason: "stat opened file", Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return openedFile{}, &PathError{Path: path, Reason: "target is not a regular file"}
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return openedFile{}, fmt.Errorf("safefile: read %q: %w", path, err)
	}
	if int64(len(content)) > MaxFileSize {
		return openedFile{}, &SizeError{Path: path, Size: int64(len(content)), Limit: MaxFileSize}
	}
	if !utf8.Valid(content) {
		return openedFile{}, &ValidationError{Path: path, Err: errors.New("content is not valid UTF-8")}
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return openedFile{}, &PathError{Path: path, Reason: "stat opened file after read", Err: err}
	}
	if !sameStableStat(stat, after) {
		return openedFile{}, &ConflictError{Path: path, Reason: "file changed while it was being read"}
	}
	return openedFile{file: File{
		RelativePath: path,
		Content:      content,
		SHA256:       shaHex(content),
		MTime:        time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec),
		Mode:         os.FileMode(stat.Mode & 0o7777),
		LineEndings:  observeLineEndings(content),
	}, stat: stat}, nil
}

func createStaged(parent int, original unix.Stat_t, content []byte) (string, int, unix.Stat_t, error) {
	for attempt := 0; attempt < 128; attempt++ {
		name, err := randomTempName()
		if err != nil {
			return "", -1, unix.Stat_t{}, err
		}
		fd, err := unix.Openat(parent, name,
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0o600,
		)
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: create staged file: %w", err)
		}
		cleanup := true
		defer func() {
			if cleanup {
				unix.Close(fd)
				_ = unix.Unlinkat(parent, name, 0)
			}
		}()
		if err := unix.Fchown(fd, int(original.Uid), int(original.Gid)); err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: copy target ownership: %w", err)
		}
		// chown may clear setuid/setgid bits, so apply the original mode last.
		if err := unix.Fchmod(fd, original.Mode&0o7777); err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: copy target mode: %w", err)
		}
		if err := writeAll(fd, content); err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: write staged file: %w", err)
		}
		if err := unix.Fsync(fd); err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: sync staged file: %w", err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return "", -1, unix.Stat_t{}, fmt.Errorf("safefile: stat staged file: %w", err)
		}
		cleanup = false
		return name, fd, stat, nil
	}
	return "", -1, unix.Stat_t{}, errors.New("safefile: could not allocate random staged filename")
}

func randomTempName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("safefile: random staged name: %w", err)
	}
	return ".dockpilot-stage-" + hex.EncodeToString(raw[:]), nil
}

func writeAll(fd int, content []byte) error {
	for len(content) != 0 {
		written, err := unix.Write(fd, content)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func verifyNamedFile(parent int, name string, expected unix.Stat_t, expectedHash string) error {
	return verifyNamedFileMatching(parent, name, expected, expectedHash, sameStableStat)
}

func verifyRenamedFile(parent int, name string, expected unix.Stat_t, expectedHash string) error {
	return verifyNamedFileMatching(parent, name, expected, expectedHash, func(left, right unix.Stat_t) bool {
		return sameFile(left, right) &&
			left.Mode == right.Mode && left.Nlink == right.Nlink &&
			left.Uid == right.Uid && left.Gid == right.Gid && left.Size == right.Size
	})
}

func verifyNamedFileMatching(parent int, name string, expected unix.Stat_t, expectedHash string, matches func(unix.Stat_t, unix.Stat_t) bool) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if !matches(stat, expected) || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("file identity changed")
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return err
	}
	if int64(len(content)) > MaxFileSize || shaHex(content) != expectedHash {
		return errors.New("file content changed")
	}
	return nil
}

func (r *Root) parentStillBound(path string, expected int) bool {
	parent, _, err := r.walkParent(path)
	if err != nil {
		return false
	}
	defer unix.Close(parent)
	return sameFD(parent, expected)
}

func sameFD(left, right int) bool {
	var leftStat, rightStat unix.Stat_t
	return unix.Fstat(left, &leftStat) == nil && unix.Fstat(right, &rightStat) == nil &&
		leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino
}

func sameFile(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func sameStableStat(left, right unix.Stat_t) bool {
	return sameFile(left, right) &&
		left.Mode == right.Mode && left.Nlink == right.Nlink &&
		left.Uid == right.Uid && left.Gid == right.Gid &&
		left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func syncDir(fd int) error { return unix.Fsync(fd) }

func shaHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func observeLineEndings(content []byte) LineEndings {
	crlf := bytes.Count(content, []byte("\r\n"))
	lf := bytes.Count(content, []byte("\n")) - crlf
	switch {
	case crlf > 0 && lf > 0:
		return LineEndingsMixed
	case crlf > 0:
		return LineEndingsCRLF
	case lf > 0:
		return LineEndingsLF
	default:
		return LineEndingsNone
	}
}

func preserveLineEndings(content []byte, endings LineEndings) []byte {
	copy := append([]byte(nil), content...)
	switch endings {
	case LineEndingsCRLF:
		copy = bytes.ReplaceAll(copy, []byte("\r\n"), []byte("\n"))
		copy = bytes.ReplaceAll(copy, []byte("\n"), []byte("\r\n"))
	case LineEndingsLF:
		copy = bytes.ReplaceAll(copy, []byte("\r\n"), []byte("\n"))
	}
	return copy
}

func cloneFile(file File) File {
	file.Content = append([]byte(nil), file.Content...)
	return file
}
