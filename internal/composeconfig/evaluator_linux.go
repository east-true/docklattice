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
	Project  composeexec.Project
	Services []string
	// EnvFiles contains the distinct service env_file paths from Compose's
	// resolved model. It never contains environment values or file contents.
	EnvFiles []string
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
	args := []string{"compose", "--progress", "plain", "--project-directory", workingDir}
	for _, file := range files {
		args = append(args, "--file", file)
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
		return Result{}, fmt.Errorf("composeconfig: start Docker Compose: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	waitErr, canceled, killErr := waitProcess(ctx, cmd, waited, grace)
	if killErr != nil {
		return Result{}, killErr
	}
	if canceled {
		return Result{}, ctx.Err()
	}
	var exitError *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &exitError) {
			return Result{}, fmt.Errorf("composeconfig: Docker Compose config exited with status %d", exitError.ExitCode())
		}
		return Result{}, fmt.Errorf("composeconfig: wait: %w", waitErr)
	}
	if stdout.tooLarge {
		clear(stdout.data)
		return Result{}, ErrOutputTooLarge
	}
	payload := stdout.data
	defer clear(payload)
	var model struct {
		Name     string `json:"name"`
		Services map[string]struct {
			EnvFile json.RawMessage `json:"env_file"`
		} `json:"services"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&model); err != nil {
		return Result{}, fmt.Errorf("composeconfig: decode Docker Compose JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Result{}, errors.New("composeconfig: trailing Docker Compose JSON")
		}
		return Result{}, fmt.Errorf("composeconfig: trailing Docker Compose JSON: %w", err)
	}
	services := make([]string, 0, len(model.Services))
	for service := range model.Services {
		services = append(services, service)
	}
	sort.Strings(services)
	envFiles, err := collectEnvFiles(model.Services, services)
	if err != nil {
		return Result{}, err
	}
	project := composeexec.Project{WorkingDir: workingDir, Files: files, Name: model.Name}
	if _, err := composeexec.BuildArgs(composeexec.Spec{Operation: composeexec.OperationConfig, Project: project}); err != nil {
		return Result{}, fmt.Errorf("%w: resolved Compose identity: %v", ErrInvalidProject, err)
	}
	return Result{Project: project, Services: services, EnvFiles: envFiles}, nil
}

func collectEnvFiles(services map[string]struct {
	EnvFile json.RawMessage `json:"env_file"`
}, names []string) ([]string, error) {
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
