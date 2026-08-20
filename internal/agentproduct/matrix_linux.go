//go:build linux

package agentproduct

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// probeFilesystem answers two questions about one path: which filesystem it is
// on, and how much room that filesystem has. The device identity comes from
// stat rather than statfs because a filesystem ID is optional and is zero on
// some filesystems, while the device number is what makes two paths the same
// filesystem.
func probeFilesystem(path string) (filesystemUsage, error) {
	var identity unix.Stat_t
	if err := unix.Stat(path, &identity); err != nil {
		return filesystemUsage{}, fmt.Errorf("stat %s: %w", path, err)
	}
	var space unix.Statfs_t
	if err := unix.Statfs(path, &space); err != nil {
		return filesystemUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	blockSize := uint64(space.Bsize)
	total, ok := filesystemBytes(space.Blocks, blockSize)
	if !ok {
		return filesystemUsage{}, fmt.Errorf("filesystem size of %s overflows", path)
	}
	free, ok := filesystemBytes(space.Bavail, blockSize)
	if !ok {
		return filesystemUsage{}, fmt.Errorf("free space of %s overflows", path)
	}
	return filesystemUsage{Device: uint64(identity.Dev), TotalBytes: total, FreeBytes: free}, nil
}

func filesystemBytes(blocks, size uint64) (uint64, bool) {
	if size != 0 && blocks > math.MaxUint64/size {
		return 0, false
	}
	return blocks * size, true
}
