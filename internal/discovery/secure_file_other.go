//go:build !linux

package discovery

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

type fileFacts struct {
	Size    int64
	ModTime time.Time
	SHA256  string
}

func secureFileFacts(context.Context, string, string) (fileFacts, error) {
	return fileFacts{}, &ScanError{Code: CodeUnsafePath, Err: errors.New("secure discovery hashing requires Linux openat/O_NOFOLLOW")}
}

func secureFileFactsBudgeted(context.Context, string, string, func(), func() error) (fileFacts, error) {
	return fileFacts{}, &ScanError{Code: CodeUnsafePath, Err: errors.New("secure discovery hashing requires Linux openat/O_NOFOLLOW")}
}

func secureDirectoryEntries(string, string) ([]fs.DirEntry, error) {
	return nil, &ScanError{Code: CodeUnsafePath, Err: errors.New("secure discovery traversal requires Linux openat/O_NOFOLLOW")}
}
