//go:build linux

package safefile

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type faultHooks struct {
	afterStageSync   func() error
	beforeFinalCheck func() error
	beforeRename     func() error
	beforeDirSync    func() error
}

// FilesystemSpace reads capacity from the same opened root inode used by
// subsequent staging. A path rename/symlink swap after OpenRoot therefore
// cannot redirect admission to another filesystem.
func (r *Root) FilesystemSpace(ctx context.Context) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, 0, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(r.rootFD, &stat); err != nil {
		return 0, 0, err
	}
	total, ok := rootFilesystemBytes(stat.Blocks, uint64(stat.Bsize))
	if !ok || total == 0 {
		return 0, 0, &PathError{Path: r.path, Reason: "filesystem size overflows int64"}
	}
	free, ok := rootFilesystemBytes(stat.Bavail, uint64(stat.Bsize))
	if !ok {
		return 0, 0, &PathError{Path: r.path, Reason: "filesystem free space overflows int64"}
	}
	return total, free, nil
}

func rootFilesystemBytes(blocks, size uint64) (int64, bool) {
	if size != 0 && blocks > uint64(math.MaxInt64)/size {
		return 0, false
	}
	return int64(blocks * size), true
}

// Root is an opened, verified project-directory capability. Every child path
// operation starts from rootFD and never re-opens an absolute target path.
type Root struct {
	mu       sync.Mutex
	path     string
	rootFD   int
	stageMu  *sync.Mutex
	release  func()
	approved map[string]Access
	closed   bool
	hooks    faultHooks
}

type sharedRootLock struct {
	mu   sync.Mutex
	refs int
}

type rootLockKey struct {
	device uint64
	inode  uint64
}

var rootLockRegistry = struct {
	sync.Mutex
	items map[rootLockKey]*sharedRootLock
}{items: make(map[rootLockKey]*sharedRootLock)}

func acquireRootLock(fd int) (*sync.Mutex, func(), error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, nil, err
	}
	key := rootLockKey{device: uint64(stat.Dev), inode: stat.Ino}
	rootLockRegistry.Lock()
	entry := rootLockRegistry.items[key]
	if entry == nil {
		entry = &sharedRootLock{}
		rootLockRegistry.items[key] = entry
	}
	entry.refs++
	rootLockRegistry.Unlock()
	return &entry.mu, func() {
		rootLockRegistry.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(rootLockRegistry.items, key)
		}
		rootLockRegistry.Unlock()
	}, nil
}

// OpenRoot verifies and opens an absolute canonical project root. approved
// entries augment the default root-level compose/env allowlist.
func OpenRoot(projectRoot string, approved []ApprovedFile) (*Root, error) {
	return openRootWithHooks(projectRoot, approved, faultHooks{})
}

// VerifyReadOnly confirms that an allowlisted reference is currently a
// regular file reachable without following any symlink. Unlike Read, it never
// reads the file's contents, so discovery can approve a Compose env_file
// without materializing secrets in memory.
func (r *Root) VerifyReadOnly(ctx context.Context, relativePath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := r.authorize(relativePath, false)
	if err != nil {
		return err
	}
	parent, _, err := r.walkParent(path)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	name := filepath.Base(path)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return &PathError{Path: path, Reason: "open regular file without following symlinks", Err: err}
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return &PathError{Path: path, Reason: "stat opened file", Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return &PathError{Path: path, Reason: "target is not a regular file"}
	}
	return ctx.Err()
}

func openRootWithHooks(projectRoot string, approved []ApprovedFile, hooks faultHooks) (*Root, error) {
	if !filepath.IsAbs(projectRoot) || filepath.Clean(projectRoot) != projectRoot {
		return nil, &PathError{Path: projectRoot, Reason: "project root must be absolute and clean"}
	}
	info, err := os.Lstat(projectRoot)
	if err != nil {
		return nil, &PathError{Path: projectRoot, Reason: "inspect project root", Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &PathError{Path: projectRoot, Reason: "project root is a symlink"}
	}
	if !info.IsDir() {
		return nil, &PathError{Path: projectRoot, Reason: "project root is not a directory"}
	}
	fd, err := unix.Open(projectRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &PathError{Path: projectRoot, Reason: "open project root", Err: err}
	}

	allow := make(map[string]Access, len(approved))
	for _, entry := range approved {
		normalized, err := normalizeRelative(entry.RelativePath)
		if err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("approved file: %w", err)
		}
		if entry.Access != ReadOnly && entry.Access != ReadWrite {
			unix.Close(fd)
			return nil, &PathError{Path: entry.RelativePath, Reason: "invalid allowlist access"}
		}
		allow[normalized] = entry.Access
	}
	stageMu, release, err := acquireRootLock(fd)
	if err != nil {
		unix.Close(fd)
		return nil, &PathError{Path: projectRoot, Reason: "identify project root", Err: err}
	}
	return &Root{path: projectRoot, rootFD: fd, stageMu: stageMu, release: release, approved: allow, hooks: hooks}, nil
}

func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := unix.Close(r.rootFD)
	r.release()
	r.release = nil
	return err
}
