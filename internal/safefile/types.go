//go:build linux

package safefile

import (
	"context"
	"io/fs"
	"time"
)

const MaxFileSize int64 = 1 << 20

type Access uint8

const (
	ReadOnly Access = iota + 1
	ReadWrite
)

// ApprovedFile adds a Server-approved reference file to the default project
// file allowlist. Referenced files should normally use ReadOnly.
type ApprovedFile struct {
	RelativePath string
	Access       Access
}

type LineEndings string

const (
	LineEndingsNone  LineEndings = "none"
	LineEndingsLF    LineEndings = "lf"
	LineEndingsCRLF  LineEndings = "crlf"
	LineEndingsMixed LineEndings = "mixed"
)

// File is the bounded content and metadata returned to the Server.
type File struct {
	RelativePath string
	Content      []byte
	SHA256       string
	MTime        time.Time
	Mode         fs.FileMode
	LineEndings  LineEndings
}

// Digest is a content-free observation for configuration inputs that may
// contain secrets. It lets discovery track drift without returning file bytes.
type Digest struct {
	RelativePath string
	Size         int64
	SHA256       string
}

type ValidationInput struct {
	ProjectRoot        string
	RelativePath       string
	StagedRelativePath string
	StagedPath         string
	StagedBytes        []byte
}

type Validator func(context.Context, ValidationInput) error

type SnapshotInput struct {
	ProjectRoot  string
	RelativePath string
	Original     File
}

type Snapshotter func(context.Context, SnapshotInput) error

// CommitGate serializes a file write's atomic rename with the owning
// Operation's BEFORE_COMMIT cancellation decision.
type CommitGate func(context.Context) error

type WriteRequest struct {
	RelativePath   string
	ExpectedSHA256 string
	Content        []byte
	Validate       Validator
	Snapshot       Snapshotter
	Commit         CommitGate
}
