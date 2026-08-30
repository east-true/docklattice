//go:build !linux

package composeconfig

import (
	"context"
	"errors"
	"time"

	"github.com/east-true/docklattice/internal/composeexec"
)

const (
	DefaultMaxOutput   = 1 << 20
	DefaultCancelGrace = 10 * time.Second
)

var (
	ErrInvalidProject = errors.New("invalid Compose project input")
	ErrOutputTooLarge = errors.New("Compose config output exceeds limit")
	ErrUnsupported    = errors.New("Compose config evaluation requires Linux process groups")
)

type Evaluator struct {
	DockerPath  string
	Env         []string
	MaxOutput   int
	CancelGrace time.Duration
}

type Result struct {
	Project        composeexec.Project
	Services       []string
	ServiceModels  []composeexec.Service
	ActiveProfiles []string
	EnvFiles       []string
	Secrets        []ResourceSource
	Configs        []ResourceSource
}

type ResourceSource struct {
	Name       string
	SourceType string
	Source     string
	External   bool
}

func (Evaluator) Evaluate(context.Context, string, []string) (Result, error) {
	return Result{}, ErrUnsupported
}
