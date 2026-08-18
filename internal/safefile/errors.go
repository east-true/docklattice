//go:build linux

package safefile

import (
	"errors"
	"fmt"
)

var (
	ErrPath       = errors.New("SAFE_FILE_PATH_REJECTED")
	ErrConflict   = errors.New("SAFE_FILE_CONFLICT")
	ErrValidation = errors.New("SAFE_FILE_VALIDATION_FAILED")
	ErrSize       = errors.New("SAFE_FILE_SIZE_EXCEEDED")
	ErrSnapshot   = errors.New("SAFE_FILE_SNAPSHOT_FAILED")
	ErrClosed     = errors.New("SAFE_FILE_ROOT_CLOSED")
)

type PathError struct {
	Path   string
	Reason string
	Err    error
}

func (e *PathError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %q: %s: %v", ErrPath, e.Path, e.Reason, e.Err)
	}
	return fmt.Sprintf("%s: %q: %s", ErrPath, e.Path, e.Reason)
}
func (e *PathError) Unwrap() error { return ErrPath }

type ConflictError struct {
	Path           string
	ExpectedSHA256 string
	Current        *File
	Reason         string
}

func (e *ConflictError) Error() string {
	current := "missing"
	if e.Current != nil {
		current = e.Current.SHA256
	}
	return fmt.Sprintf("%s: %q: expected %s, current %s: %s",
		ErrConflict, e.Path, e.ExpectedSHA256, current, e.Reason)
}
func (e *ConflictError) Unwrap() error { return ErrConflict }

type ValidationError struct {
	Path string
	Err  error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %q: %v", ErrValidation, e.Path, e.Err)
}
func (e *ValidationError) Unwrap() error { return ErrValidation }

type SizeError struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *SizeError) Error() string {
	return fmt.Sprintf("%s: %q is %d bytes, limit %d", ErrSize, e.Path, e.Size, e.Limit)
}
func (e *SizeError) Unwrap() error { return ErrSize }

type SnapshotError struct {
	Path string
	Err  error
}

func (e *SnapshotError) Error() string {
	return fmt.Sprintf("%s: %q: %v", ErrSnapshot, e.Path, e.Err)
}
func (e *SnapshotError) Unwrap() error { return ErrSnapshot }
