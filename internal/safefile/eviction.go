//go:build linux

package safefile

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// ReclaimAbandonedStagingForDiskPressure removes crash-left safefile staging
// objects from this verified project root. All Root instances for the same
// canonical path share stageMu, so a live Write can never be selected. Restore
// staging uses a different prefix and is deliberately outside this boundary.
func (r *Root) ReclaimAbandonedStagingForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	r.stageMu.Lock()
	defer r.stageMu.Unlock()

	directoryFD, err := unix.Openat(r.rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("safefile: open staging directory: %w", err)
	}
	directory := os.NewFile(uintptr(directoryFD), r.path)
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var freed int64
	for _, entry := range entries {
		if freed >= bytesNeeded {
			break
		}
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		name := entry.Name()
		if !validStagingName(name) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(r.rootFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return freed, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Size < 0 {
			return freed, &PathError{Path: name, Reason: "unsafe abandoned staging object"}
		}
		if err := unix.Unlinkat(r.rootFD, name, 0); err != nil {
			return freed, err
		}
		freed += stat.Size
	}
	if freed > 0 {
		if err := syncDir(r.rootFD); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

func validStagingName(name string) bool {
	const prefix = ".docklattice-stage-"
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 {
		return false
	}
	for _, char := range name[len(prefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
