//go:build linux

// Package composeconfig delegates Compose model resolution and validation to
// the bundled Docker Compose CLI. It never parses YAML itself.
package composeconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/east-true/dockpilot/internal/composeexec"
)

const (
	DefaultMaxOutput   = 1 << 20
	DefaultCancelGrace = 10 * time.Second
)

var (
	ErrInvalidProject = errors.New("invalid Compose project input")
	ErrOutputTooLarge = errors.New("Compose config output exceeds limit")
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
	// EnvFiles contains the distinct service env_file paths from Compose's
	// resolved model. It never contains environment values or file contents.
	EnvFiles []string
	Secrets  []ResourceSource
	Configs  []ResourceSource
}

// ResourceSource is content-free Compose secret/config source metadata. Source
// may be a relative file path, an environment variable name, or an external
// resource name; it never contains the referenced value or file content.
type ResourceSource struct {
	Name       string
	SourceType string
	Source     string
	External   bool
}

type composeModel struct {
	Name     string                     `json:"name"`
	Services map[string]rawServiceModel `json:"services"`
	Secrets  map[string]rawResource     `json:"secrets"`
	Configs  map[string]rawResource     `json:"configs"`
}

type rawServiceModel struct {
	Image      string          `json:"image"`
	Build      json.RawMessage `json:"build"`
	PullPolicy string          `json:"pull_policy"`
	Profiles   []string        `json:"profiles"`
	DependsOn  json.RawMessage `json:"depends_on"`
	EnvFile    json.RawMessage `json:"env_file"`
}

type rawResource struct {
	File        string          `json:"file"`
	Environment string          `json:"environment"`
	Name        string          `json:"name"`
	External    json.RawMessage `json:"external"`
}

func (e Evaluator) Evaluate(ctx context.Context, workingDir string, files []string) (Result, error) {
	workingDir, files, err := validateInputs(workingDir, files)
	if err != nil {
		return Result{}, err
	}
	dockerPath := e.DockerPath
	if dockerPath == "" {
		dockerPath = "docker"
	}
	limit := e.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	grace := e.CancelGrace
	if grace <= 0 {
		grace = DefaultCancelGrace
	}
	active, err := e.evaluateModel(ctx, dockerPath, workingDir, files, false, limit, grace)
	if err != nil {
		return Result{}, err
	}
	all, err := e.evaluateModel(ctx, dockerPath, workingDir, files, true, limit, grace)
	if err != nil {
		return Result{}, err
	}
	if active.Name == "" || all.Name != active.Name {
		return Result{}, fmt.Errorf("%w: inconsistent Compose project name", ErrInvalidProject)
	}
	activeNames := make(map[string]struct{}, len(active.Services))
	for name := range active.Services {
		activeNames[name] = struct{}{}
	}
	services := sortedServiceNames(all.Services)
	models, activeProfiles, err := serviceModels(all.Services, activeNames, services)
	if err != nil {
		return Result{}, err
	}
	envFiles, err := collectEnvFiles(all.Services, services)
	if err != nil {
		return Result{}, err
	}
	secrets, err := resourceSources("secret", all.Secrets)
	if err != nil {
		return Result{}, err
	}
	configs, err := resourceSources("config", all.Configs)
	if err != nil {
		return Result{}, err
	}
	project := composeexec.Project{WorkingDir: workingDir, Files: files, Name: active.Name, Services: models}
	if _, err := composeexec.BuildArgs(composeexec.Spec{Operation: composeexec.OperationConfig, Project: project}); err != nil {
		return Result{}, fmt.Errorf("%w: resolved Compose identity: %v", ErrInvalidProject, err)
	}
	return Result{
		Project: project, Services: services, ServiceModels: models, ActiveProfiles: activeProfiles,
		EnvFiles: envFiles, Secrets: secrets, Configs: configs,
	}, nil
}

func (e Evaluator) evaluateModel(ctx context.Context, dockerPath, workingDir string, files []string, allProfiles bool, limit int, grace time.Duration) (composeModel, error) {
	args := []string{"compose", "--progress", "plain", "--project-directory", workingDir}
	for _, file := range files {
		args = append(args, "--file", file)
	}
	if allProfiles {
		args = append(args, "--profile", "*")
	}
	args = append(args, "config", "--format", "json", "--no-env-resolution")

	stdout := &boundedWriter{limit: limit}
	stderr := composeexec.NewTailWriter(64 << 10)
	cmd := exec.Command(dockerPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e.Env != nil {
		cmd.Env = append([]string(nil), e.Env...)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return composeModel{}, fmt.Errorf("composeconfig: start Docker Compose: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	waitErr, canceled, killErr := waitProcess(ctx, cmd, waited, grace)
	if killErr != nil {
		return composeModel{}, killErr
	}
	if canceled {
		return composeModel{}, ctx.Err()
	}
	var exitError *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &exitError) {
			return composeModel{}, fmt.Errorf("composeconfig: Docker Compose config exited with status %d", exitError.ExitCode())
		}
		return composeModel{}, fmt.Errorf("composeconfig: wait: %w", waitErr)
	}
	if stdout.tooLarge {
		clear(stdout.data)
		return composeModel{}, ErrOutputTooLarge
	}
	payload := stdout.data
	defer clear(payload)
	var model composeModel
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&model); err != nil {
		return composeModel{}, fmt.Errorf("composeconfig: decode Docker Compose JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return composeModel{}, errors.New("composeconfig: trailing Docker Compose JSON")
		}
		return composeModel{}, fmt.Errorf("composeconfig: trailing Docker Compose JSON: %w", err)
	}
	return model, nil
}

func sortedServiceNames(services map[string]rawServiceModel) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func serviceModels(services map[string]rawServiceModel, active map[string]struct{}, names []string) ([]composeexec.Service, []string, error) {
	models := make([]composeexec.Service, 0, len(names))
	profileSet := make(map[string]struct{})
	for _, name := range names {
		raw := services[name]
		dependsOn, err := parseDependsOn(raw.DependsOn)
		if err != nil {
			return nil, nil, fmt.Errorf("composeconfig: decode service %q depends_on: %w", name, err)
		}
		profiles := append([]string(nil), raw.Profiles...)
		sort.Strings(profiles)
		_, isActive := active[name]
		if isActive {
			for _, profile := range profiles {
				profileSet[profile] = struct{}{}
			}
		}
		models = append(models, composeexec.Service{
			Name: name, Image: raw.Image, HasBuild: hasJSONValue(raw.Build), PullPolicy: raw.PullPolicy,
			Profiles: profiles, DependsOn: dependsOn, Active: isActive,
		})
	}
	activeProfiles := make([]string, 0, len(profileSet))
	for profile := range profileSet {
		activeProfiles = append(activeProfiles, profile)
	}
	sort.Strings(activeProfiles)
	return models, activeProfiles, nil
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

func parseDependsOn(raw json.RawMessage) ([]string, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	var names []string
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &names); err != nil {
			return nil, errors.New("invalid list")
		}
	} else {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, errors.New("invalid map")
		}
		for name := range values {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func resourceSources(kind string, resources map[string]rawResource) ([]ResourceSource, error) {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]ResourceSource, 0, len(names))
	for _, name := range names {
		raw := resources[name]
		external, err := externalResource(raw.External)
		if err != nil {
			return nil, fmt.Errorf("composeconfig: decode %s %q external metadata: %w", kind, name, err)
		}
		sourceType, source := "", ""
		switch {
		case raw.File != "":
			sourceType, source = "file", raw.File
		case raw.Environment != "":
			sourceType, source = "environment", raw.Environment
		case external:
			sourceType, source = "external", raw.Name
			if source == "" {
				source = name
			}
		}
		result = append(result, ResourceSource{Name: name, SourceType: sourceType, Source: source, External: external})
	}
	return result, nil
}

func externalResource(raw json.RawMessage) (bool, error) {
	if !hasJSONValue(raw) {
		return false, nil
	}
	var boolean bool
	if err := json.Unmarshal(raw, &boolean); err == nil {
		return boolean, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		return true, nil
	}
	return false, errors.New("invalid value")
}

func collectEnvFiles(services map[string]rawServiceModel, names []string) ([]string, error) {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, name := range names {
		references, err := parseEnvFileField(services[name].EnvFile)
		if err != nil {
			return nil, fmt.Errorf("composeconfig: decode service %q env_file: %w", name, err)
		}
		for _, reference := range references {
			if _, exists := seen[reference]; exists {
				continue
			}
			seen[reference] = struct{}{}
			result = append(result, reference)
		}
	}
	sort.Strings(result)
	return result, nil
}

func parseEnvFileField(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty value")
	}
	if trimmed[0] == '[' {
		var entries []json.RawMessage
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, errors.New("invalid list")
		}
		result := make([]string, 0, len(entries))
		for _, entry := range entries {
			path, err := parseEnvFileEntry(entry)
			if err != nil {
				return nil, err
			}
			result = append(result, path)
		}
		return result, nil
	}
	path, err := parseEnvFileEntry(trimmed)
	if err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func parseEnvFileEntry(raw json.RawMessage) (string, error) {
	var path string
	if err := json.Unmarshal(raw, &path); err == nil {
		return validateEnvFilePath(path)
	}
	var entry struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Path == nil {
		return "", errors.New("entry must be a path string or path object")
	}
	return validateEnvFilePath(*entry.Path)
}

func validateEnvFilePath(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("path is empty or contains NUL")
	}
	return path, nil
}

func validateInputs(workingDir string, files []string) (string, []string, error) {
	if workingDir == "" || !filepath.IsAbs(workingDir) || strings.ContainsRune(workingDir, 0) || len(files) == 0 || len(files) > 32 {
		return "", nil, ErrInvalidProject
	}
	workingDir = filepath.Clean(workingDir)
	result := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file == "" || !filepath.IsAbs(file) || strings.ContainsRune(file, 0) {
			return "", nil, ErrInvalidProject
		}
		file = filepath.Clean(file)
		if file != workingDir && !strings.HasPrefix(file, workingDir+string(filepath.Separator)) {
			return "", nil, ErrInvalidProject
		}
		if _, duplicate := seen[file]; duplicate {
			continue
		}
		seen[file] = struct{}{}
		result = append(result, file)
	}
	sort.Strings(result)
	return workingDir, result, nil
}

type boundedWriter struct {
	mu       sync.Mutex
	limit    int
	data     []byte
	tooLarge bool
}

func (w *boundedWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - len(w.data)
	if remaining > len(payload) {
		remaining = len(payload)
	}
	if remaining > 0 {
		w.data = append(w.data, payload[:remaining]...)
	}
	if remaining < len(payload) {
		w.tooLarge = true
	}
	return len(payload), nil
}

func waitProcess(ctx context.Context, cmd *exec.Cmd, waited <-chan error, grace time.Duration) (error, bool, error) {
	select {
	case err := <-waited:
		return err, false, nil
	case <-ctx.Done():
		select {
		case err := <-waited:
			return err, false, nil
		default:
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, true, fmt.Errorf("composeconfig: terminate process group: %w", err)
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case err := <-waited:
			return err, true, nil
		case <-timer.C:
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return nil, true, fmt.Errorf("composeconfig: kill process group: %w", err)
			}
			return <-waited, true, nil
		}
	}
}
