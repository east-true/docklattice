//go:build linux

package backup

import (
	"context"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func openedRootFilesystemSpace(ctx context.Context, root *os.Root) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return 0, 0, err
	}
	defer directory.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(directory.Fd()), &stat); err != nil {
		return 0, 0, err
	}
	total, ok := filesystemBytes(stat.Blocks, uint64(stat.Bsize))
	if !ok || total == 0 {
		return 0, 0, fmt.Errorf("backup: project filesystem size overflows int64")
	}
	free, ok := filesystemBytes(stat.Bavail, uint64(stat.Bsize))
	if !ok {
		return 0, 0, fmt.Errorf("backup: project filesystem free space overflows int64")
	}
	return total, free, nil
}

func filesystemBytes(blocks, size uint64) (int64, bool) {
	if size != 0 && blocks > uint64(math.MaxInt64)/size {
		return 0, false
	}
	return int64(blocks * size), true
}
