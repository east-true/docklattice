// Package operation implements Dockpilot's transport- and persistence-neutral
// Operation state, cancellation, locking, idempotency, and bounded result
// retention rules.
package operation

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Type string

const (
	TypeContainerStart   Type = "container.start"
	TypeContainerStop    Type = "container.stop"
	TypeContainerRestart Type = "container.restart"
	TypeContainerRemove  Type = "container.remove"
	TypeComposePull      Type = "compose.pull"
	TypeComposeUp        Type = "compose.up"
	TypeComposeDown      Type = "compose.down"
	TypeComposeStart     Type = "compose.start"
	TypeComposeStop      Type = "compose.stop"
	TypeComposeRestart   Type = "compose.restart"
	TypeComposeFileWrite Type = "compose.file.write"
	TypeEnvWrite         Type = "env.write"
	TypeOverrideWrite    Type = "override.write"
	TypeBackupCreate     Type = "backup.create"
	TypeBackupRestore    Type = "backup.restore"
	TypeDiscoveryRescan  Type = "discovery.rescan"
)

func (t Type) Valid() bool {
	switch t {
	case TypeContainerStart, TypeContainerStop, TypeContainerRestart, TypeContainerRemove,
		TypeComposePull, TypeComposeUp, TypeComposeDown, TypeComposeStart,
		TypeComposeStop, TypeComposeRestart, TypeComposeFileWrite, TypeEnvWrite,
		TypeOverrideWrite, TypeBackupCreate, TypeBackupRestore, TypeDiscoveryRescan:
		return true
	default:
		return false
	}
}

type Status string

const (
	StatusRequested   Status = "requested"
	StatusDispatched  Status = "dispatched"
	StatusRunning     Status = "running"
	StatusSuccess     Status = "success"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
	StatusInterrupted Status = "interrupted"
	StatusUnknown     Status = "unknown"
	StatusRejected    Status = "rejected"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusCanceled, StatusInterrupted, StatusRejected:
		return true
	default:
		return false
	}
}

type Phase string

const (
	PhasePreparing  Phase = "PREPARING"
	PhaseExecuting  Phase = "EXECUTING"
	PhaseCommitting Phase = "COMMITTING"
	PhaseFinalizing Phase = "FINALIZING"
)

type CancelMode string

const (
	CancelNone              CancelMode = "NONE"
	CancelSafe              CancelMode = "SAFE"
	CancelBestEffortPartial CancelMode = "BEST_EFFORT_PARTIAL"
	CancelBeforeCommit      CancelMode = "BEFORE_COMMIT"
)

type CancelReason string

const (
	CancelReasonUser          CancelReason = "USER"
	CancelReasonTimeout       CancelReason = "TIMEOUT"
	CancelReasonAgentShutdown CancelReason = "AGENT_SHUTDOWN"
)

type CancelOutcome string

const (
	CancelAccepted        CancelOutcome = "ACCEPTED"
	CancelTooLate         CancelOutcome = "TOO_LATE"
	CancelNotCancelable   CancelOutcome = "NOT_CANCELABLE"
	CancelAlreadyTerminal CancelOutcome = "ALREADY_TERMINAL"
	CancelNotFound        CancelOutcome = "NOT_FOUND"
)

type ErrorCode string

const (
	CodeInvalidSpec       ErrorCode = "INVALID_SPEC"
	CodeSpecMismatch      ErrorCode = "SPEC_MISMATCH"
	CodeIllegalTransition ErrorCode = "ILLEGAL_TRANSITION"
	CodeProjectBusy       ErrorCode = "PROJECT_BUSY"
	CodeAgentShuttingDown ErrorCode = "AGENT_SHUTTING_DOWN"
)

// ManagedAuditDelivery is the durable outbox state stored in the same
// minimal journal record as the terminal Operation transition.
type ManagedAuditDelivery string

const (
	ManagedAuditNone      ManagedAuditDelivery = ""
	ManagedAuditPending   ManagedAuditDelivery = "PENDING"
	ManagedAuditDelivered ManagedAuditDelivery = "DELIVERED"
)

// Error is a typed engine error suitable for stable API mapping.
type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func HasErrorCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func validPayloadHash(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Spec is the immutable identity of an Operation. Reusing an operation_id with
// any different field is rejected rather than treated as an idempotent retry.
type Spec struct {
	OperationID string
	ProjectKey  string
	Target      string
	Type        Type
	// PayloadHash binds transport-specific structured options into exact-spec
	// idempotency without persisting secret-bearing request content.
	PayloadHash string
}

// Record is a point-in-time, copy-safe Operation snapshot.
type Record struct {
	OperationID string
	ProjectKey  string
	Target      string
	Type        Type
	PayloadHash string

	RequestedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time

	Status          Status
	Phase           Phase
	CancelMode      CancelMode
	Revision        uint64
	CommitStartedAt time.Time
	LastProgressAt  time.Time
	StalledWarning  bool

	CancelRequestedAt      time.Time
	CancelReason           CancelReason
	PartialEffectsPossible bool

	Result          string
	Error           string
	OutputTail      []byte
	OutputTruncated bool

	ManagedAuditDelivery ManagedAuditDelivery
}

func cancelModeForType(operationType Type) CancelMode {
	switch operationType {
	case TypeDiscoveryRescan, TypeComposePull, TypeBackupCreate:
		return CancelSafe
	case TypeComposeUp, TypeComposeDown, TypeComposeStart, TypeComposeStop, TypeComposeRestart:
		return CancelBestEffortPartial
	case TypeComposeFileWrite, TypeEnvWrite, TypeOverrideWrite, TypeBackupRestore,
		TypeContainerStart, TypeContainerStop, TypeContainerRestart, TypeContainerRemove:
		return CancelBeforeCommit
	default:
		return CancelNone
	}
}

func requiresProjectLock(operationType Type) bool {
	switch operationType {
	case TypeDiscoveryRescan:
		return false
	default:
		return true
	}
}
