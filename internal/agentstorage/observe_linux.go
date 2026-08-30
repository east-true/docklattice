//go:build linux

package agentstorage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"

	"github.com/east-true/docklattice/internal/diskbudget"
	"golang.org/x/sys/unix"
)

func observeFilesystem(ctx context.Context, root string) (diskbudget.Observation, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return diskbudget.Observation{}, err
	}
	total, ok := multiplyUint64(stat.Blocks, uint64(stat.Bsize))
	if !ok || total == 0 {
		return diskbudget.Observation{}, fmt.Errorf("filesystem size overflows int64")
	}
	free, ok := multiplyUint64(stat.Bavail, uint64(stat.Bsize))
	if !ok {
		return diskbudget.Observation{}, fmt.Errorf("filesystem free space overflows int64")
	}
	var used int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Atomic state updates may remove a staging file between WalkDir's
			// directory read and its metadata lookup.  It is not durable state
			// loss; continue the observation and let the next refresh see it.
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Size() > math.MaxInt64-used {
			return fmt.Errorf("Agent state usage overflows int64")
		}
		used += info.Size()
		return nil
	})
	if err != nil {
		return diskbudget.Observation{}, err
	}
	return diskbudget.Observation{FilesystemTotalBytes: total, FilesystemFreeBytes: free, AgentStateBytes: used}, nil
}

func multiplyUint64(left, right uint64) (int64, bool) {
	if right != 0 && left > uint64(math.MaxInt64)/right {
		return 0, false
	}
	return int64(left * right), true
}
