package agentstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxStateFileSize = 1 << 20

type persistHooks struct {
	beforeFileSync func() error
	beforeRename   func() error
	beforeDirSync  func() error
}

func ensureSecureDir(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("agentstate: create state directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("agentstate: inspect state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: state directory %q", ErrSymlink, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("agentstate: state path %q is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: state directory %q mode is %04o, want 0700",
			ErrInsecureMode, dir, info.Mode().Perm())
	}
	return nil
}

func acquireLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path,
		syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, fmt.Errorf("%w: lock file %q", ErrSymlink, path)
		}
		return nil, fmt.Errorf("agentstate: open lock file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("agentstate: inspect lock file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, fmt.Errorf("%w: lock file %q mode is %v", ErrInsecureMode, path, info.Mode())
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("%w: %q", ErrStateLocked, path)
		}
		return nil, fmt.Errorf("agentstate: lock state: %w", err)
	}
	return file, nil
}

func releaseLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("agentstate: unlock state: %w", unlockErr)
	}
	return closeErr
}

func readState(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("agentstate: inspect state file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%w: state file %q", ErrSymlink, path)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, false, fmt.Errorf("%w: state file %q mode is %v", ErrInsecureMode, path, info.Mode())
	}

	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, false, fmt.Errorf("%w: state file %q", ErrSymlink, path)
		}
		return nil, false, fmt.Errorf("agentstate: open state file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileSize+1))
	if err != nil {
		return nil, false, fmt.Errorf("agentstate: read state file: %w", err)
	}
	if len(data) > maxStateFileSize {
		return nil, false, fmt.Errorf("%w: state file exceeds %d bytes", ErrStateInvariant, maxStateFileSize)
	}
	return data, true, nil
}

func decodeState(data []byte, dst *diskState) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: decode state: %v", ErrStateInvariant, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: trailing state data", ErrStateInvariant)
	}
	return nil
}

func writeStateAtomic(ctx context.Context, dir, path string, state diskState, hooks persistHooks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("agentstate: encode state: %w", err)
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(dir, ".agent-state-*.tmp")
	if err != nil {
		return fmt.Errorf("agentstate: create temporary state: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("agentstate: chmod temporary state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("agentstate: write temporary state: %w", err)
	}
	if hooks.beforeFileSync != nil {
		if err := hooks.beforeFileSync(); err != nil {
			return fmt.Errorf("agentstate: injected file sync failure: %w", err)
		}
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("agentstate: sync temporary state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("agentstate: close temporary state: %w", err)
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: state file %q", ErrSymlink, path)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("%w: state file %q mode is %v", ErrInsecureMode, path, info.Mode())
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("agentstate: inspect state before replace: %w", err)
	}
	if hooks.beforeRename != nil {
		if err := hooks.beforeRename(); err != nil {
			return fmt.Errorf("agentstate: injected rename failure: %w", err)
		}
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("agentstate: replace state: %w", err)
	}
	committed = true

	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("agentstate: open state directory for sync: %w", err)
	}
	defer directory.Close()
	if hooks.beforeDirSync != nil {
		if err := hooks.beforeDirSync(); err != nil {
			return fmt.Errorf("agentstate: injected directory sync failure: %w", err)
		}
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("agentstate: sync state directory: %w", err)
	}
	return nil
}

func cleanTempFiles(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".agent-state-*.tmp"))
	for _, match := range matches {
		_ = os.Remove(match)
	}
}
