package agentproduct

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/east-true/dockpilot/internal/agentops"
	"github.com/east-true/dockpilot/internal/composeexec"
	"github.com/east-true/dockpilot/internal/logrelay"
)

const (
	maxComposeLogServices = 256
	maxComposeLogTail     = 10_000
	composeLogRelayChunks = 128
)

var (
	composeLogProjectUIDRE = regexp.MustCompile(`^[a-f0-9]{64}$`)
	composeLogService      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// composeLogSource keeps project/service logs on the same bounded P3 relay as
// container logs. The typed Request fields prevent a project UID from being
// treated as a container ID or an arbitrary Docker/Compose argument.
type composeLogSource struct {
	docker    logrelay.Source
	inventory Docker
	compose   agentops.Compose
	projects  Projects
}

func (source composeLogSource) Stream(ctx context.Context, request logrelay.Request, emit func(logrelay.Chunk) error) error {
	if request.ProjectUID == "" {
		return source.docker.Stream(ctx, request, emit)
	}
	if err := validateComposeLogRequest(request); err != nil {
		return err
	}
	projectSnapshot, found := source.projects.ProjectSnapshot(request.ProjectUID)
	if !found || projectSnapshot.Stale || !projectSnapshot.ComposeExecutable {
		return errors.New("agentproduct: Compose project is unavailable")
	}
	if request.ContainerID != "" {
		if len(request.Services) != 0 || source.inventory == nil {
			return errors.New("agentproduct: Container is not attached to the Compose project")
		}
		container, err := source.inventory.Inspect(ctx, request.ContainerID)
		if err != nil || filepath.Clean(container.Labels["com.docker.compose.project.working_dir"]) != filepath.Clean(projectSnapshot.WorkingDir) ||
			container.Labels["com.docker.compose.project"] != projectSnapshot.Name {
			return errors.New("agentproduct: Container is not attached to the Compose project")
		}
		request.ProjectUID = ""
		return source.docker.Stream(ctx, request, emit)
	}
	if err := validateKnownServices(request.Services, projectSnapshot.Services); err != nil {
		return err
	}
	project, found, err := source.projects.Project(ctx, request.ProjectUID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("agentproduct: Compose project is unavailable")
	}
	output := make(chan composeexec.OutputChunk, composeLogRelayChunks)
	type completed struct {
		result composeexec.Result
		err    error
	}
	done := make(chan completed, 1)
	go func() {
		result, runErr := source.compose.Run(ctx, composeexec.Spec{
			Operation: composeexec.OperationLogs,
			Project:   project,
			Services:  append([]string(nil), request.Services...),
			Flags: composeexec.Flags{
				LogsFollow: request.Follow, LogsTail: int(request.TailLines), LogsTimestamps: request.Timestamps,
				LogsSince: request.Since, LogsUntil: request.Until,
			},
			OutputTailBytes: 1,
		}, output)
		close(output)
		done <- completed{result: result, err: runErr}
	}()

	var relayedDrops uint64
	for chunk := range output {
		if len(chunk.Data) > 0 {
			stream := logrelay.Stdout
			if chunk.Stream == composeexec.StreamStderr {
				stream = logrelay.Stderr
			}
			if err := emit(logrelay.Chunk{Data: chunk.Data, Stream: stream}); err != nil {
				return err
			}
		}
		if chunk.DroppedBytes > 0 {
			relayedDrops += chunk.DroppedBytes
			if err := emit(logrelay.Chunk{DroppedBytes: chunk.DroppedBytes}); err != nil {
				return err
			}
		}
	}
	completedRun := <-done
	defer clear(completedRun.result.Tail)
	if completedRun.result.RelayDroppedBytes > relayedDrops {
		if err := emit(logrelay.Chunk{DroppedBytes: completedRun.result.RelayDroppedBytes - relayedDrops}); err != nil {
			return err
		}
	}
	if completedRun.err != nil {
		return fmt.Errorf("agentproduct: docker compose logs: %w", completedRun.err)
	}
	if completedRun.result.Canceled {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("agentproduct: docker compose logs canceled")
	}
	if completedRun.result.ExitCode != 0 {
		return fmt.Errorf("agentproduct: docker compose logs exited with status %d", completedRun.result.ExitCode)
	}
	return nil
}

func validateComposeLogRequest(request logrelay.Request) error {
	if !composeLogProjectUIDRE.MatchString(request.ProjectUID) || request.ProjectUID == "" {
		return errors.New("agentproduct: Compose log project UID is invalid")
	}
	if len(request.Services) > maxComposeLogServices || request.TailLines > maxComposeLogTail {
		return errors.New("agentproduct: Compose log request exceeds safe limits")
	}
	// Compose output does not preserve the stdout/stderr classification of each
	// service line. Do not accept an option the CLI cannot truthfully honor.
	if !request.ShowStdout || !request.ShowStderr {
		return errors.New("agentproduct: Compose logs require stdout and stderr together")
	}
	return nil
}

func validateKnownServices(requested, available []string) error {
	known := make(map[string]struct{}, len(available))
	for _, service := range available {
		known[service] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	for _, service := range requested {
		if len(service) > 128 || !composeLogService.MatchString(service) {
			return errors.New("agentproduct: Compose log service is invalid")
		}
		if _, duplicate := seen[service]; duplicate {
			return errors.New("agentproduct: duplicate Compose log service")
		}
		if _, found := known[service]; !found {
			return errors.New("agentproduct: Compose log service is not part of the project")
		}
		seen[service] = struct{}{}
	}
	return nil
}
