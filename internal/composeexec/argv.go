package composeexec

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Operation string

const (
	OperationPS      Operation = "ps"
	OperationPull    Operation = "pull"
	OperationUp      Operation = "up"
	OperationDown    Operation = "down"
	OperationStart   Operation = "start"
	OperationStop    Operation = "stop"
	OperationRestart Operation = "restart"
	OperationLogs    Operation = "logs"
	OperationConfig  Operation = "config"
)

type PullPolicy string

const (
	PullPolicyDefault PullPolicy = ""
	PullPolicyAlways  PullPolicy = "always"
	PullPolicyMissing PullPolicy = "missing"
	PullPolicyNever   PullPolicy = "never"
)

type ConfigOutput string

const (
	ConfigOutputDefault ConfigOutput = ""
	ConfigOutputJSON    ConfigOutput = "json"
	ConfigOutputQuiet   ConfigOutput = "quiet"
)

var (
	ErrInvalidSpec = errors.New("invalid compose execution specification")
	projectNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	serviceNameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// Project identifies a previously discovered Compose project. Paths must have
// already crossed the Agent's discovery-root/path-safety boundary.
type Project struct {
	WorkingDir string
	Files      []string
	Name       string
	// Services is the bounded effective Compose model used by the Agent to
	// enforce DockLattice's no-build mutation policy. Build metadata never becomes
	// an executable build surface.
	Services []Service
	// EnvFile is an Agent-resolved validation-only override. It is never
	// populated directly from a remote absolute path.
	EnvFile string
}

// Service is content-free execution policy metadata resolved by Docker
// Compose. Active means the Service belongs to a project-level invocation with
// the currently active profiles. A targeted Service may bring dependencies
// into scope, so DependsOn is retained for policy validation.
type Service struct {
	Name       string
	Image      string
	HasBuild   bool
	PullPolicy string
	Profiles   []string
	DependsOn  []string
	Active     bool
}

func (service Service) BuildRequired() bool {
	return service.Image == "" || service.HasBuild && service.PullPolicy == "build"
}

func (service Service) ImageBacked() bool {
	return service.Image != "" && !service.BuildRequired()
}

// Flags is intentionally closed: adding a Compose flag requires an explicit
// product decision and an argv-builder change.
type Flags struct {
	RemoveOrphans  bool
	ForceRecreate  bool
	Pull           PullPolicy
	LogsFollow     bool
	LogsTail       int
	LogsTimestamps bool
	LogsSince      string
	LogsUntil      string
	PSAll          bool
	ConfigOutput   ConfigOutput
	// ConfigNoInterpolate is available only for a browser-facing, read-only
	// config view. It avoids materializing project environment values.
	ConfigNoInterpolate bool
}

type Spec struct {
	Operation Operation
	Project   Project
	Services  []string
	Flags     Flags
	// OutputTailBytes is selected by trusted Agent code, never decoded from a
	// remote request. Queries may use a bounded capture larger than persisted
	// operation output without changing the 64 KiB operation-tail policy.
	OutputTailBytes int
}

// BuildArgs returns arguments for a fixed `docker compose` invocation. It never
// emits a shell command or accepts raw flags.
func BuildArgs(spec Spec) ([]string, error) {
	if !validOperation(spec.Operation) {
		return nil, fmt.Errorf("%w: unsupported operation %q", ErrInvalidSpec, spec.Operation)
	}
	if err := validateProject(spec.Project); err != nil {
		return nil, err
	}
	if err := validateServices(spec.Operation, spec.Services); err != nil {
		return nil, err
	}
	if err := validateFlags(spec.Operation, spec.Flags); err != nil {
		return nil, err
	}

	args := []string{"compose", "--progress", "plain", "--project-directory", filepath.Clean(spec.Project.WorkingDir)}
	if spec.Project.EnvFile != "" {
		args = append(args, "--env-file", filepath.Clean(spec.Project.EnvFile))
	}
	for _, file := range spec.Project.Files {
		args = append(args, "--file", filepath.Clean(file))
	}
	args = append(args, "--project-name", spec.Project.Name, string(spec.Operation))

	switch spec.Operation {
	case OperationUp:
		// DockLattice v1 never builds Images. This flag is unconditional rather
		// than caller-selectable so no API path can accidentally opt into build.
		args = append(args, "--detach", "--no-build")
		if spec.Flags.RemoveOrphans {
			args = append(args, "--remove-orphans")
		}
		if spec.Flags.ForceRecreate {
			args = append(args, "--force-recreate")
		}
		if spec.Flags.Pull != PullPolicyDefault {
			args = append(args, "--pull", string(spec.Flags.Pull))
		}
	case OperationDown:
		if spec.Flags.RemoveOrphans {
			args = append(args, "--remove-orphans")
		}
	case OperationLogs:
		if spec.Flags.LogsFollow {
			args = append(args, "--follow")
		}
		if spec.Flags.LogsTail > 0 {
			args = append(args, "--tail", fmt.Sprintf("%d", spec.Flags.LogsTail))
		}
		if spec.Flags.LogsTimestamps {
			args = append(args, "--timestamps")
		}
		if spec.Flags.LogsSince != "" {
			args = append(args, "--since", spec.Flags.LogsSince)
		}
		if spec.Flags.LogsUntil != "" {
			args = append(args, "--until", spec.Flags.LogsUntil)
		}
	case OperationPS:
		if spec.Flags.PSAll {
			args = append(args, "--all")
		}
	case OperationConfig:
		if spec.Flags.ConfigNoInterpolate {
			args = append(args, "--no-interpolate")
		}
		switch spec.Flags.ConfigOutput {
		case ConfigOutputJSON:
			args = append(args, "--format", "json")
		case ConfigOutputQuiet:
			args = append(args, "--quiet")
		}
	}
	args = append(args, spec.Services...)
	return args, nil
}

func validOperation(operation Operation) bool {
	switch operation {
	case OperationPS, OperationPull, OperationUp, OperationDown, OperationStart, OperationStop, OperationRestart, OperationLogs, OperationConfig:
		return true
	default:
		return false
	}
}

func validateProject(project Project) error {
	if project.WorkingDir == "" || !filepath.IsAbs(project.WorkingDir) || strings.ContainsRune(project.WorkingDir, 0) {
		return fmt.Errorf("%w: working directory must be an absolute safe path", ErrInvalidSpec)
	}
	if len(project.Files) == 0 || len(project.Files) > 32 {
		return fmt.Errorf("%w: compose files count must be 1..32", ErrInvalidSpec)
	}
	if project.EnvFile != "" && (!filepath.IsAbs(project.EnvFile) || strings.ContainsRune(project.EnvFile, 0)) {
		return fmt.Errorf("%w: env file must be an absolute safe path", ErrInvalidSpec)
	}
	for _, file := range project.Files {
		if file == "" || !filepath.IsAbs(file) || strings.ContainsRune(file, 0) {
			return fmt.Errorf("%w: compose file must be an absolute safe path", ErrInvalidSpec)
		}
	}
	if len(project.Name) > 63 || !projectNameRE.MatchString(project.Name) {
		return fmt.Errorf("%w: invalid project name", ErrInvalidSpec)
	}
	return nil
}

func validateServices(operation Operation, services []string) error {
	if operation == OperationDown && len(services) != 0 {
		return fmt.Errorf("%w: %s is project-only", ErrInvalidSpec, operation)
	}
	if len(services) > 256 {
		return fmt.Errorf("%w: too many services", ErrInvalidSpec)
	}
	for _, service := range services {
		if len(service) > 128 || !serviceNameRE.MatchString(service) {
			return fmt.Errorf("%w: invalid service target %q", ErrInvalidSpec, service)
		}
	}
	return nil
}

func validateFlags(operation Operation, flags Flags) error {
	if flags.RemoveOrphans && operation != OperationUp && operation != OperationDown {
		return fmt.Errorf("%w: remove-orphans is not valid for %s", ErrInvalidSpec, operation)
	}
	if flags.ForceRecreate && operation != OperationUp {
		return fmt.Errorf("%w: force-recreate is not valid for %s", ErrInvalidSpec, operation)
	}
	if flags.Pull != PullPolicyDefault {
		if operation != OperationUp || (flags.Pull != PullPolicyAlways && flags.Pull != PullPolicyMissing && flags.Pull != PullPolicyNever) {
			return fmt.Errorf("%w: invalid pull policy", ErrInvalidSpec)
		}
	}
	if (flags.LogsFollow || flags.LogsTail != 0 || flags.LogsTimestamps || flags.LogsSince != "" || flags.LogsUntil != "") && operation != OperationLogs {
		return fmt.Errorf("%w: log flags are not valid for %s", ErrInvalidSpec, operation)
	}
	if flags.LogsTail < 0 {
		return fmt.Errorf("%w: logs tail must be non-negative", ErrInvalidSpec)
	}
	var since, until time.Time
	var err error
	if flags.LogsSince != "" {
		since, err = time.Parse(time.RFC3339Nano, flags.LogsSince)
		if err != nil {
			return fmt.Errorf("%w: invalid logs since time", ErrInvalidSpec)
		}
	}
	if flags.LogsUntil != "" {
		until, err = time.Parse(time.RFC3339Nano, flags.LogsUntil)
		if err != nil {
			return fmt.Errorf("%w: invalid logs until time", ErrInvalidSpec)
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return fmt.Errorf("%w: logs since must not be after until", ErrInvalidSpec)
	}
	if flags.PSAll && operation != OperationPS {
		return fmt.Errorf("%w: ps-all is not valid for %s", ErrInvalidSpec, operation)
	}
	if flags.ConfigOutput != ConfigOutputDefault {
		if operation != OperationConfig || (flags.ConfigOutput != ConfigOutputJSON && flags.ConfigOutput != ConfigOutputQuiet) {
			return fmt.Errorf("%w: invalid config output mode", ErrInvalidSpec)
		}
	}
	if flags.ConfigNoInterpolate && operation != OperationConfig {
		return fmt.Errorf("%w: no-interpolate is not valid for %s", ErrInvalidSpec, operation)
	}
	return nil
}
