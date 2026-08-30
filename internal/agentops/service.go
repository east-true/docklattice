// Package agentops adapts product operation requests to the Agent-authoritative
// operation engine and fixed Docker/Compose executors.
package agentops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/east-true/docklattice/internal/composeexec"
	productconfig "github.com/east-true/docklattice/internal/config"
	"github.com/east-true/docklattice/internal/dockeradapter"
	"github.com/east-true/docklattice/internal/operation"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/safefile"
)

var (
	ErrUnsupportedOperation = errors.New("agent operation is not wired to a safe executor")
	ErrProjectUnavailable   = errors.New("Agent project is unavailable or not safely managed")
	ErrComposeBuildRequired = errors.New("Compose mutation requires an Image build, which DockLattice v1 does not perform")
)

type Docker interface {
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Restart(context.Context, string) error
	Remove(context.Context, string, dockeradapter.RemoveOptions) error
}

type Compose interface {
	Run(context.Context, composeexec.Spec, chan<- composeexec.OutputChunk) (composeexec.Result, error)
}

type ProjectCatalog interface {
	Project(context.Context, string) (composeexec.Project, bool, error)
}

// FileApprovalCatalog exposes only catalog-owned, read-only reference paths.
// It never accepts a remote path and returns a defensive immutable snapshot.
type FileApprovalCatalog interface {
	ApprovedReadOnlyFiles(context.Context, string) ([]safefile.ApprovedFile, bool, error)
}

type FilesystemPolicy interface {
	FilesystemMutationAllowed(context.Context, string) (bool, string)
}

type Rescanner interface {
	Rescan(context.Context) error
	RescanProject(context.Context, string) error
}

type Config struct {
	Engine     *operation.Engine
	Docker     Docker
	Compose    Compose
	Projects   ProjectCatalog
	Approvals  FileApprovalCatalog
	Filesystem FilesystemPolicy
	Rescanner  Rescanner
	Backups    BackupManager
	Admission  DiskAdmitter
	Timeouts   productconfig.OperationTimeouts
}

type Service struct{ config Config }

var _ producttransport.OperationHandler = (*Service)(nil)

func New(config Config) (*Service, error) {
	if config.Engine == nil || config.Docker == nil || config.Compose == nil || config.Projects == nil || config.Approvals == nil || config.Filesystem == nil || config.Rescanner == nil ||
		config.Backups == nil || config.Admission == nil {
		return nil, errors.New("agentops: engine and all safe executor boundaries are required")
	}
	if config.Timeouts.Container <= 0 {
		config.Timeouts = productconfig.V1Defaults().OperationTimeout
	}
	return &Service{config: config}, nil
}

func (s *Service) StartOperation(ctx context.Context, _ producttransport.SessionInfo, request producttransport.OperationRequest) (producttransport.OperationResponse, error) {
	if len(request.OperationID) == 0 || len(request.OperationID) > 256 || len(request.ProjectKey) > 1024 || len(request.Target) > 1024 {
		return producttransport.OperationResponse{}, fmt.Errorf("agentops: invalid operation identity")
	}
	opType := operation.Type(request.Type)
	if !opType.Valid() {
		return producttransport.OperationResponse{}, fmt.Errorf("agentops: unsupported operation type %q", request.Type)
	}
	if !s.supported(opType) {
		return producttransport.OperationResponse{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, opType)
	}
	if (isFileWriteOperation(opType) || isBackupOperation(opType)) &&
		(!safeOpaqueID.MatchString(request.OperationID) || request.OperationID == "." || request.OperationID == ".." ||
			!safeOpaqueID.MatchString(request.ProjectKey) || request.ProjectKey == "." || request.ProjectKey == "..") {
		return producttransport.OperationResponse{}, errors.New("agentops: mutation operation_id and project_uid must be safe opaque identifiers")
	}
	command, err := parseOperationCommand(opType, request.Target, request.Payload)
	if err != nil {
		return producttransport.OperationResponse{}, err
	}
	projectKey := request.ProjectKey
	if isContainerOperation(opType) && projectKey == "" {
		projectKey = "container:" + request.Target
	}
	var project composeexec.Project
	var approvedFiles []safefile.ApprovedFile
	if isManagedProjectOperation(opType) {
		if request.ProjectKey == "" {
			return producttransport.OperationResponse{}, fmt.Errorf("agentops: operation requires project_uid")
		}
		var ok bool
		if project, ok, err = s.config.Projects.Project(ctx, request.ProjectKey); err != nil {
			return producttransport.OperationResponse{}, err
		} else if !ok {
			return producttransport.OperationResponse{}, ErrProjectUnavailable
		}
		if isFileWriteOperation(opType) || opType == operation.TypeBackupRestore {
			allowed, reason := s.config.Filesystem.FilesystemMutationAllowed(ctx, request.ProjectKey)
			if !allowed {
				return producttransport.OperationResponse{}, fmt.Errorf("%w: filesystem mutation denied: %s", ErrProjectUnavailable, reason)
			}
		}
		if isFileWriteOperation(opType) {
			var available bool
			approvedFiles, available, err = s.config.Approvals.ApprovedReadOnlyFiles(ctx, request.ProjectKey)
			if err != nil {
				return producttransport.OperationResponse{}, err
			}
			if !available {
				return producttransport.OperationResponse{}, ErrProjectUnavailable
			}
			approvedFiles = append([]safefile.ApprovedFile(nil), approvedFiles...)
		}
		if isComposeOperation(opType) {
			command.composeServices, err = composeMutationServices(opType, request.Target, project.Services)
			if err != nil {
				return producttransport.OperationResponse{}, err
			}
		}
	}
	digest := sha256.Sum256(request.Payload)
	spec := operation.Spec{
		OperationID: request.OperationID, ProjectKey: projectKey, Target: request.Target,
		Type: opType, PayloadHash: hex.EncodeToString(digest[:]),
	}
	record, _, err := s.config.Engine.StartOperation(ctx, spec, func(runCtx context.Context, current *operation.Operation) {
		s.run(runCtx, current, command, project, approvedFiles)
	})
	if err != nil {
		return producttransport.OperationResponse{}, err
	}
	return responseFromRecord(record), nil
}

func (s *Service) supported(kind operation.Type) bool {
	return isContainerOperation(kind) || isComposeOperation(kind) || isFileWriteOperation(kind) || isBackupOperation(kind) || kind == operation.TypeDiscoveryRescan
}

func isManagedProjectOperation(kind operation.Type) bool {
	return isComposeOperation(kind) || isFileWriteOperation(kind) || isBackupOperation(kind)
}

func isContainerOperation(kind operation.Type) bool {
	switch kind {
	case operation.TypeContainerStart, operation.TypeContainerStop, operation.TypeContainerRestart, operation.TypeContainerRemove:
		return true
	default:
		return false
	}
}

func isComposeOperation(kind operation.Type) bool {
	switch kind {
	case operation.TypeComposePull, operation.TypeComposeUp, operation.TypeComposeDown,
		operation.TypeComposeStart, operation.TypeComposeStop, operation.TypeComposeRestart:
		return true
	default:
		return false
	}
}

func (s *Service) run(runCtx context.Context, current *operation.Operation, command operationCommand, project composeexec.Project, approvedFiles []safefile.ApprovedFile) {
	record := current.Snapshot()
	if budgetKind, ok := diskOperation(record.Type); ok {
		if err := s.config.Admission.AdmitOperation(runCtx, budgetKind); err != nil {
			_ = current.Reject(err)
			return
		}
	}
	if err := current.TransitionStatus(operation.StatusRunning, "", ""); err != nil {
		return
	}
	if err := current.AdvancePhase(operation.PhaseExecuting); err != nil {
		_ = current.TransitionStatus(operation.StatusFailed, "", err.Error())
		return
	}
	record = current.Snapshot()
	timeout := s.timeout(record.Type)
	timer := time.AfterFunc(timeout, func() {
		_, _ = s.config.Engine.CancelWithError(record.OperationID, operation.CancelReasonTimeout)
	})
	defer timer.Stop()

	var result string
	var runErr error
	switch {
	case isContainerOperation(record.Type):
		if err := current.EnterCommit(); err != nil {
			s.finishCanceledOrFailed(runCtx, current, err)
			return
		}
		runErr = s.runContainer(runCtx, record)
	case isComposeOperation(record.Type):
		result, runErr = s.runCompose(runCtx, current, record, project, command.composeServices)
	case isFileWriteOperation(record.Type):
		result, runErr = s.runFileWrite(runCtx, current, project, approvedFiles, *command.fileWrite)
	case record.Type == operation.TypeBackupCreate:
		result, runErr = s.runBackupCreate(runCtx, current, project, *command.backupCreate)
	case record.Type == operation.TypeBackupRestore:
		result, runErr = s.runBackupRestore(runCtx, current, project, command.backupID)
	case record.Type == operation.TypeDiscoveryRescan:
		runErr = s.config.Rescanner.Rescan(runCtx)
	}
	if runErr == nil && runCtx.Err() == nil && requiresTargetedProjectRefresh(record.Type) {
		runErr = s.config.Rescanner.RescanProject(runCtx, record.ProjectKey)
	}
	if runErr != nil || runCtx.Err() != nil {
		committing := current.Snapshot().Phase == operation.PhaseCommitting
		if runErr != nil || !committing {
			if runErr == nil {
				runErr = runCtx.Err()
			}
			s.finishCanceledOrFailed(runCtx, current, runErr)
			return
		}
	}
	timer.Stop()
	if err := current.AdvancePhase(operation.PhaseFinalizing); err != nil {
		_ = current.TransitionStatus(operation.StatusFailed, "", err.Error())
		return
	}
	_ = current.TransitionStatus(operation.StatusSuccess, result, "")
	_ = current.FlushOutputTail()
}

func requiresTargetedProjectRefresh(kind operation.Type) bool {
	switch kind {
	case operation.TypeComposeUp, operation.TypeComposeFileWrite, operation.TypeEnvWrite,
		operation.TypeOverrideWrite, operation.TypeBackupRestore:
		return true
	default:
		return false
	}
}

func (s *Service) runContainer(ctx context.Context, record operation.Record) error {
	switch record.Type {
	case operation.TypeContainerStart:
		return s.config.Docker.Start(ctx, record.Target)
	case operation.TypeContainerStop:
		return s.config.Docker.Stop(ctx, record.Target)
	case operation.TypeContainerRestart:
		return s.config.Docker.Restart(ctx, record.Target)
	case operation.TypeContainerRemove:
		return s.config.Docker.Remove(ctx, record.Target, dockeradapter.RemoveOptions{})
	default:
		return ErrUnsupportedOperation
	}
}

func (s *Service) runCompose(ctx context.Context, current *operation.Operation, record operation.Record, project composeexec.Project, services []string) (string, error) {
	composeOperation := mapComposeOperation(record.Type)
	relay := make(chan composeexec.OutputChunk, 32)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for chunk := range relay {
			if chunk.DroppedBytes > 0 {
				_, _ = current.WriteOutput([]byte(fmt.Sprintf("\n[docklattice: %d output bytes omitted]\n", chunk.DroppedBytes)))
			}
			_, _ = current.WriteOutput(chunk.Data)
		}
	}()
	result, runErr := s.config.Compose.Run(ctx, composeexec.Spec{
		Operation: composeOperation, Project: project, Services: services,
	}, relay)
	close(relay)
	<-drained
	// The runner's own always-drained tail is authoritative if live relay
	// chunks were dropped.
	_, _ = current.WriteOutput(result.Tail)
	if runErr != nil {
		return "", runErr
	}
	if !result.Success() {
		if result.Canceled {
			return "", context.Canceled
		}
		return "", fmt.Errorf("docker compose %s exited with status %d", composeOperation, result.ExitCode)
	}
	return fmt.Sprintf("docker compose %s completed", composeOperation), nil
}

func composeMutationServices(kind operation.Type, target string, models []composeexec.Service) ([]string, error) {
	policy, err := composeexec.EvaluateV1Policy(models)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComposeBuildRequired, err)
	}
	services, err := policy.Targets(mapComposeOperation(kind), target)
	if err != nil {
		if kind == operation.TypeComposePull || kind == operation.TypeComposeUp {
			return nil, fmt.Errorf("%w: %v", ErrComposeBuildRequired, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrProjectUnavailable, err)
	}
	return services, nil
}

func mapComposeOperation(kind operation.Type) composeexec.Operation {
	return composeexec.Operation(strings.TrimPrefix(string(kind), "compose."))
}

func (s *Service) finishCanceledOrFailed(ctx context.Context, current *operation.Operation, runErr error) {
	snapshot := current.Snapshot()
	if ctx.Err() == nil && snapshot.CancelRequestedAt.IsZero() {
		_ = current.TransitionStatus(operation.StatusFailed, "", runErr.Error())
		return
	}
	reason := snapshot.CancelReason
	if reason == "" {
		reason = operation.CancelReasonUser
	}
	outcome, cancelErr := s.config.Engine.CancelWithError(current.Snapshot().OperationID, reason)
	if cancelErr != nil {
		runErr = errors.Join(runErr, cancelErr)
	}
	if outcome == operation.CancelAccepted {
		_ = current.TransitionStatus(operation.StatusCanceled, "", runErr.Error())
		return
	}
	_ = current.TransitionStatus(operation.StatusFailed, "", runErr.Error())
}

func (s *Service) timeout(kind operation.Type) time.Duration {
	switch kind {
	case operation.TypeContainerStart, operation.TypeContainerStop, operation.TypeContainerRestart, operation.TypeContainerRemove:
		return s.config.Timeouts.Container
	case operation.TypeComposeUp:
		return s.config.Timeouts.ComposeUp
	case operation.TypeComposeRestart:
		return s.config.Timeouts.ComposeRestart
	case operation.TypeComposeDown:
		return s.config.Timeouts.ComposeDown
	case operation.TypeComposePull:
		return s.config.Timeouts.ComposePull
	case operation.TypeComposeFileWrite, operation.TypeEnvWrite, operation.TypeOverrideWrite:
		return s.config.Timeouts.FileWrite
	case operation.TypeBackupCreate:
		return s.config.Timeouts.BackupCreate
	case operation.TypeBackupRestore:
		return s.config.Timeouts.BackupRestore
	case operation.TypeDiscoveryRescan:
		return s.config.Timeouts.DiscoveryRescan
	default:
		return s.config.Timeouts.Container
	}
}

func responseFromRecord(record operation.Record) producttransport.OperationResponse {
	canCancel, cancelReason := operationCancelability(record)
	return producttransport.OperationResponse{
		Status: string(record.Status), Phase: string(record.Phase), Revision: record.Revision,
		PartialEffectsPossible: record.PartialEffectsPossible, Error: record.Error,
		OutputTail: append([]byte(nil), record.OutputTail...), OutputTruncated: record.OutputTruncated,
		CancelMode: string(record.CancelMode), CanCancel: canCancel, CancelabilityReason: cancelReason,
		RequestedAt: record.RequestedAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
	}
}

func operationCancelability(record operation.Record) (bool, string) {
	if record.Status.Terminal() {
		return false, "operation is terminal"
	}
	if !record.CancelRequestedAt.IsZero() {
		return false, "cancellation already requested"
	}
	if record.CancelMode == operation.CancelNone {
		return false, "operation is not cancelable"
	}
	if record.CancelMode == operation.CancelBeforeCommit && !record.CommitStartedAt.IsZero() {
		return false, "commit has started"
	}
	return true, ""
}
