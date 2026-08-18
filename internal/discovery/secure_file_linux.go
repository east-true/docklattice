//go:build linux

package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type fileFacts struct {
	Size    int64
	ModTime time.Time
	SHA256  string
}

func secureFileFacts(ctx context.Context, root, relative string) (fileFacts, error) {
	return secureFileFactsBudgeted(ctx, root, relative, nil, nil)
}

// secureFileFactsWithHook exists so tests can deterministically mutate a file
// between hashing and the final fstat without weakening the production API.
func secureFileFactsWithHook(ctx context.Context, root, relative string, afterRead func()) (fileFacts, error) {
	return secureFileFactsBudgeted(ctx, root, relative, afterRead, nil)
}

func secureFileFactsBudgeted(ctx context.Context, root, relative string, afterRead func(), budgetCheck func() error) (fileFacts, error) {
	if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fileFacts{}, &ScanError{Code: CodeUnsafePath, Path: relative, Err: errors.New("path is outside the discovery root")}
	}
	cleanRelative := filepath.Clean(relative)
	if cleanRelative != relative {
		return fileFacts{}, &ScanError{Code: CodeUnsafePath, Path: relative, Err: errors.New("path is not canonical")}
	}

	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	currentFD, err := openAbsoluteDirectory(root)
	if err != nil {
		return fileFacts{}, classifyOpenError(root, err)
	}
	parts := strings.Split(cleanRelative, string(filepath.Separator))
	for _, component := range parts[:len(parts)-1] {
		nextFD, openErr := syscall.Openat(currentFD, component, flags|syscall.O_DIRECTORY, 0)
		_ = syscall.Close(currentFD)
		if openErr != nil {
			return fileFacts{}, classifyOpenError(filepath.Join(root, cleanRelative), openErr)
		}
		currentFD = nextFD
	}
	fileFD, err := syscall.Openat(currentFD, parts[len(parts)-1], flags, 0)
	_ = syscall.Close(currentFD)
	if err != nil {
		return fileFacts{}, classifyOpenError(filepath.Join(root, cleanRelative), err)
	}
	file := os.NewFile(uintptr(fileFD), filepath.Join(root, cleanRelative))
	if file == nil {
		_ = syscall.Close(fileFD)
		return fileFacts{}, &ScanError{Code: CodeFilesystem, Path: relative, Err: errors.New("create file descriptor wrapper")}
	}
	defer file.Close()

	var before syscall.Stat_t
	if err := syscall.Fstat(fileFD, &before); err != nil {
		return fileFacts{}, &ScanError{Code: CodeFilesystem, Path: relative, Err: fmt.Errorf("fstat before hash: %w", err)}
	}
	if before.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fileFacts{}, &ScanError{Code: CodeUnsafePath, Path: relative, Err: errors.New("compose path is not a regular file")}
	}

	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var readBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fileFacts{}, err
		}
		if budgetCheck != nil {
			if err := budgetCheck(); err != nil {
				return fileFacts{}, err
			}
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			readBytes += int64(count)
			_, _ = hash.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fileFacts{}, &ScanError{Code: CodeFilesystem, Path: relative, Err: fmt.Errorf("hash file: %w", readErr)}
		}
	}
	if afterRead != nil {
		afterRead()
	}
	var after syscall.Stat_t
	if err := syscall.Fstat(fileFD, &after); err != nil {
		return fileFacts{}, &ScanError{Code: CodeFilesystem, Path: relative, Err: fmt.Errorf("fstat after hash: %w", err)}
	}
	if readBytes != before.Size || !stableStat(before, after) {
		return fileFacts{}, &ScanError{Code: CodeFileUnstable, Path: relative, Err: errors.New("file changed while it was being hashed")}
	}
	return fileFacts{
		Size:    before.Size,
		ModTime: time.Unix(before.Mtim.Sec, before.Mtim.Nsec),
		SHA256:  hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func secureDirectoryEntries(root, relative string) ([]fs.DirEntry, error) {
	currentFD, err := openAbsoluteDirectory(root)
	if err != nil {
		return nil, classifyOpenError(root, err)
	}
	if relative != "." {
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Clean(relative) != relative {
			_ = syscall.Close(currentFD)
			return nil, &ScanError{Code: CodeUnsafePath, Path: relative, Err: errors.New("directory path is outside the discovery root")}
		}
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_DIRECTORY
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			nextFD, openErr := syscall.Openat(currentFD, component, flags, 0)
			_ = syscall.Close(currentFD)
			if openErr != nil {
				return nil, classifyOpenError(filepath.Join(root, relative), openErr)
			}
			currentFD = nextFD
		}
	}
	directory := os.NewFile(uintptr(currentFD), filepath.Join(root, relative))
	if directory == nil {
		_ = syscall.Close(currentFD)
		return nil, &ScanError{Code: CodeFilesystem, Path: relative, Err: errors.New("create directory descriptor wrapper")}
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, &ScanError{Code: CodeFilesystem, Path: relative, Err: err}
	}
	return entries, nil
}

func openAbsoluteDirectory(directory string) (int, error) {
	directory = filepath.Clean(directory)
	if !filepath.IsAbs(directory) {
		return -1, errors.New("root is not absolute")
	}
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_DIRECTORY
	currentFD, err := syscall.Open(string(filepath.Separator), flags, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(directory, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		nextFD, openErr := syscall.Openat(currentFD, component, flags, 0)
		_ = syscall.Close(currentFD)
		if openErr != nil {
			return -1, openErr
		}
		currentFD = nextFD
	}
	return currentFD, nil
}

func stableStat(before, after syscall.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Mode == after.Mode &&
		before.Nlink == after.Nlink && before.Size == after.Size &&
		before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

func classifyOpenError(path string, err error) error {
	switch {
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return &ScanError{Code: CodeUnsafePath, Path: path, Err: err}
	case errors.Is(err, syscall.ENOENT):
		return &ScanError{Code: CodeFileUnstable, Path: path, Err: err}
	default:
		return &ScanError{Code: CodeFilesystem, Path: path, Err: err}
	}
}
