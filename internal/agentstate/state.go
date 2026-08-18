package agentstate

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/east-true/dockpilot/internal/agentid"
)

type diskState struct {
	Version              int                          `json:"version"`
	AgentID              string                       `json:"agent_id"`
	Credential           Credential                   `json:"credential,omitempty"`
	PendingActivation    *PendingCredentialActivation `json:"pending_credential_activation,omitempty"`
	BoundArchive         *ArchiveBinding              `json:"bound_archive,omitempty"`
	RetiredArchives      []RetiredArchive             `json:"retired_archives,omitempty"`
	CurrentIncarnation   uint64                       `json:"current_incarnation"`
	CleanClose           *CleanClose                  `json:"clean_close,omitempty"`
	LastDockerEventAt    string                       `json:"last_docker_event_at,omitempty"`
	DockerSnapshotSHA256 string                       `json:"docker_snapshot_sha256,omitempty"`
}

// Store owns one running Agent's durable state. The process lock prevents two
// Agent instances from advancing the same incarnation concurrently.
type Store struct {
	mu       sync.Mutex
	dir      string
	path     string
	lockFile *os.File
	state    diskState
	ready    bool
	closed   bool
	hooks    persistHooks
}

// Inspect reads and validates an existing state identity without creating the
// directory, taking the process lock, advancing incarnation, or rewriting any
// bytes. Agent boot uses it before auditwal.Recover.
func Inspect(dir string) (Inspection, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return Inspection{}, fmt.Errorf("agentstate: resolve state directory: %w", err)
	}
	info, err := os.Lstat(absDir)
	if os.IsNotExist(err) {
		return Inspection{}, nil
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("agentstate: inspect state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Inspection{}, fmt.Errorf("%w: state directory %q", ErrSymlink, absDir)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return Inspection{}, fmt.Errorf("%w: state directory %q mode is %v", ErrInsecureMode, absDir, info.Mode())
	}
	data, exists, err := readState(filepath.Join(absDir, StateFileName))
	if err != nil || !exists {
		return Inspection{}, err
	}
	var state diskState
	if err := decodeState(data, &state); err != nil {
		return Inspection{}, err
	}
	if err := validateDiskState(state); err != nil {
		return Inspection{}, err
	}
	return Inspection{Exists: true, AgentID: state.AgentID, CurrentIncarnation: state.CurrentIncarnation}, nil
}

// Open loads or creates Agent state, assesses the previous clean-close marker,
// increments current_incarnation, and fsyncs the file and its directory. A
// returned Store is therefore ready for event subscription and request intake.
func Open(ctx context.Context, dir string, walTail *Cursor) (*Store, Startup, error) {
	return openWithHooks(ctx, dir, walTail, persistHooks{})
}

func openWithHooks(ctx context.Context, dir string, walTail *Cursor, hooks persistHooks) (*Store, Startup, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, Startup{}, fmt.Errorf("agentstate: resolve state directory: %w", err)
	}
	if err := ensureSecureDir(absDir); err != nil {
		return nil, Startup{}, err
	}
	lockFile, err := acquireLock(filepath.Join(absDir, ".agent-state.lock"))
	if err != nil {
		return nil, Startup{}, err
	}
	cleanupLock := true
	defer func() {
		if cleanupLock {
			_ = releaseLock(lockFile)
		}
	}()

	cleanTempFiles(absDir)
	statePath := filepath.Join(absDir, StateFileName)
	data, exists, err := readState(statePath)
	if err != nil {
		return nil, Startup{}, err
	}

	var state diskState
	if exists {
		if err := decodeState(data, &state); err != nil {
			return nil, Startup{}, err
		}
		if err := validateDiskState(state); err != nil {
			return nil, Startup{}, err
		}
	} else {
		if walTail != nil {
			return nil, Startup{}, &StateInvariantError{Reason: "WAL exists but Agent state is absent"}
		}
		agentID, err := agentid.New()
		if err != nil {
			return nil, Startup{}, err
		}
		state = diskState{Version: stateVersion, AgentID: agentID}
	}

	startup, err := assessStartup(state, walTail, exists)
	if err != nil {
		return nil, Startup{}, err
	}
	if state.CurrentIncarnation == math.MaxUint64 {
		return nil, Startup{}, &StateInvariantError{Reason: "incarnation counter exhausted"}
	}
	state.CurrentIncarnation++
	state.CleanClose = nil
	startup.AgentID = state.AgentID
	startup.CurrentIncarnation = state.CurrentIncarnation

	if err := writeStateAtomic(ctx, absDir, statePath, state, hooks); err != nil {
		return nil, Startup{}, err
	}

	store := &Store{
		dir:      absDir,
		path:     statePath,
		state:    state,
		ready:    true,
		hooks:    hooks,
		lockFile: lockFile,
	}
	cleanupLock = false
	return store, startup, nil
}

func assessStartup(state diskState, walTail *Cursor, existed bool) (Startup, error) {
	startup := Startup{PreviousIncarnation: state.CurrentIncarnation}
	if walTail != nil {
		if walTail.Incarnation == 0 || walTail.Seq == 0 {
			return Startup{}, &StateInvariantError{Reason: "WAL tail cursor contains zero component"}
		}
		if walTail.Incarnation > state.CurrentIncarnation {
			return Startup{}, &StateInvariantError{Reason: fmt.Sprintf(
				"WAL incarnation %d is ahead of state incarnation %d",
				walTail.Incarnation, state.CurrentIncarnation,
			)}
		}
		startup.KnownDurableThrough = cloneCursor(walTail)
	}
	if !existed || state.CurrentIncarnation == 0 {
		return startup, nil
	}

	expectedLastSeq := uint64(0)
	if walTail != nil && walTail.Incarnation == state.CurrentIncarnation {
		expectedLastSeq = walTail.Seq
	}
	startup.PreviousUnclean = state.CleanClose == nil ||
		state.CleanClose.Incarnation != state.CurrentIncarnation ||
		state.CleanClose.LastDurableSeq != expectedLastSeq
	return startup, nil
}

func validateDiskState(state diskState) error {
	if state.Version != stateVersion {
		return &StateInvariantError{Reason: fmt.Sprintf("unsupported state version %d", state.Version)}
	}
	if !agentid.Valid(state.AgentID) {
		return &StateInvariantError{Reason: "agent_id is not a canonical UUIDv4"}
	}
	if err := validateCredential(state.Credential); err != nil {
		return err
	}
	if state.PendingActivation != nil {
		if err := validateCredential(state.PendingActivation.Previous); err != nil {
			return err
		}
		if credentialEmpty(state.Credential) || credentialEmpty(state.PendingActivation.Previous) || state.PendingActivation.ActiveCredentialID == "" {
			return &StateInvariantError{Reason: "pending credential activation is incomplete"}
		}
	}
	if state.BoundArchive != nil {
		if err := validateBinding(*state.BoundArchive); err != nil {
			return err
		}
	} else if len(state.RetiredArchives) != 0 {
		return &StateInvariantError{Reason: "retired archives exist without an active binding"}
	}
	if state.LastDockerEventAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, state.LastDockerEventAt)
		if err != nil || parsed.IsZero() || state.LastDockerEventAt != timestamp(parsed) {
			return &StateInvariantError{Reason: "last_docker_event_at is invalid or non-canonical"}
		}
	}
	if state.DockerSnapshotSHA256 != "" && !validSHA256(state.DockerSnapshotSHA256) {
		return &StateInvariantError{Reason: "docker_snapshot_sha256 is invalid"}
	}
	var previousGeneration uint64
	archiveIDs := make(map[string]struct{}, len(state.RetiredArchives)+1)
	for index, retired := range state.RetiredArchives {
		if retired.Generation == 0 || retired.ArchiveID == "" || retired.RetiredAt == "" {
			return &StateInvariantError{Reason: fmt.Sprintf("retired archive %d is incomplete", index)}
		}
		if retired.Generation <= previousGeneration {
			return &StateInvariantError{Reason: "retired archive generations are not strictly increasing"}
		}
		if retired.AckedThrough != nil && !validCursor(*retired.AckedThrough) {
			return &StateInvariantError{Reason: fmt.Sprintf("retired archive %d ACK is invalid", index)}
		}
		if _, duplicate := archiveIDs[retired.ArchiveID]; duplicate {
			return &StateInvariantError{Reason: "an archive_id is reused across generations"}
		}
		archiveIDs[retired.ArchiveID] = struct{}{}
		previousGeneration = retired.Generation
	}
	if state.BoundArchive != nil {
		if previousGeneration >= state.BoundArchive.Generation {
			return &StateInvariantError{Reason: "active archive generation does not follow retired generations"}
		}
		if _, reused := archiveIDs[state.BoundArchive.ArchiveID]; reused {
			return &StateInvariantError{Reason: "active archive_id was used by a retired generation"}
		}
	}
	return nil
}

func validateCredential(credential Credential) error {
	if credential.FileReference != "" && len(credential.Data) != 0 {
		return &StateInvariantError{Reason: "credential has both file reference and inline data"}
	}
	return nil
}

func credentialEmpty(credential Credential) bool {
	return credential.FileReference == "" && len(credential.Data) == 0
}

func validateBinding(binding ArchiveBinding) error {
	if binding.ServerIdentityID == "" || binding.Generation == 0 || binding.ArchiveID == "" {
		return &StateInvariantError{Reason: "archive binding identity fields are incomplete"}
	}
	if !validCursor(binding.CoverageBeginsAt) {
		return &StateInvariantError{Reason: "archive coverage_begins_at is invalid"}
	}
	if binding.AckedThrough != nil && !validCursor(*binding.AckedThrough) {
		return &StateInvariantError{Reason: "archive acked_through is invalid"}
	}
	if binding.AckedThrough != nil && compareCursor(*binding.AckedThrough, binding.CoverageBeginsAt) < 0 {
		return &StateInvariantError{Reason: "archive acked_through precedes coverage_begins_at"}
	}
	return nil
}

func validCursor(cursor Cursor) bool {
	return cursor.Incarnation > 0 && cursor.Seq > 0
}

// Ready reports whether startup durability completed and the Store has not
// been closed.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && !s.closed
}

// Snapshot returns a defensive copy of the current durable state.
func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, ErrClosed
	}
	return snapshotOf(s.state), nil
}

// SetCredential atomically persists credential material. Supplying an empty
// value clears the current credential; file and inline forms are exclusive.
func (s *Store) SetCredential(ctx context.Context, credential Credential) error {
	if err := validateCredential(credential); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	next := cloneDiskState(s.state)
	next.Credential = cloneCredential(credential)
	return s.commitLocked(ctx, next)
}

// InstallCredentialAndBind atomically commits credential material and the
// archive identity returned by registration or renewal in one Agent-state
// replacement. The external request has completed, but the Agent must not use
// the credential until this durable transition succeeds.
func (s *Store) InstallCredentialAndBind(
	ctx context.Context,
	credential Credential,
	serverIdentityID string,
	generation uint64,
	archiveID string,
	coverageBeginsAt Cursor,
	now time.Time,
) (RebindResult, error) {
	if err := validateCredential(credential); err != nil {
		return RebindResult{}, err
	}
	presented := ArchiveBinding{
		ServerIdentityID: serverIdentityID, Generation: generation,
		ArchiveID: archiveID, CoverageBeginsAt: coverageBeginsAt,
	}
	if err := validateBinding(presented); err != nil {
		return RebindResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RebindResult{}, ErrClosed
	}
	if !s.ready {
		return RebindResult{}, ErrNotReady
	}
	next, result, err := applyArchiveBinding(s.state, presented, now)
	if err != nil {
		return RebindResult{}, err
	}
	next.Credential = cloneCredential(credential)
	next.PendingActivation = nil
	if err := s.commitLocked(ctx, next); err != nil {
		return RebindResult{}, err
	}
	return result, nil
}

// StageCredentialRenewalAndBind atomically makes the replacement credential
// current, preserves the previous credential for restart-safe activation, and
// applies any forward Archive Rebind returned with the renewal.
func (s *Store) StageCredentialRenewalAndBind(
	ctx context.Context,
	previous, active Credential,
	activeCredentialID string,
	serverIdentityID string,
	generation uint64,
	archiveID string,
	coverageBeginsAt Cursor,
	now time.Time,
) (RebindResult, error) {
	if credentialEmpty(previous) || credentialEmpty(active) || activeCredentialID == "" {
		return RebindResult{}, &StateInvariantError{Reason: "credential renewal stage is incomplete"}
	}
	if err := validateCredential(previous); err != nil {
		return RebindResult{}, err
	}
	if err := validateCredential(active); err != nil {
		return RebindResult{}, err
	}
	presented := ArchiveBinding{ServerIdentityID: serverIdentityID, Generation: generation, ArchiveID: archiveID, CoverageBeginsAt: coverageBeginsAt}
	if err := validateBinding(presented); err != nil {
		return RebindResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RebindResult{}, ErrClosed
	}
	if !s.ready {
		return RebindResult{}, ErrNotReady
	}
	next, result, err := applyArchiveBinding(s.state, presented, now)
	if err != nil {
		return RebindResult{}, err
	}
	next.Credential = cloneCredential(active)
	next.PendingActivation = &PendingCredentialActivation{
		Previous: cloneCredential(previous), ActiveCredentialID: activeCredentialID,
	}
	if err := s.commitLocked(ctx, next); err != nil {
		return RebindResult{}, err
	}
	return result, nil
}

// CompleteCredentialActivation removes the previous bearer credential only
// after the Server confirms Activate. It is idempotent for an already-cleared
// marker and rejects clearing a different staged replacement.
func (s *Store) CompleteCredentialActivation(ctx context.Context, activeCredentialID string) error {
	if activeCredentialID == "" {
		return &StateInvariantError{Reason: "active credential ID is empty"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	if s.state.PendingActivation == nil {
		return nil
	}
	if s.state.PendingActivation.ActiveCredentialID != activeCredentialID {
		return &StateInvariantError{Reason: "credential activation ID does not match pending stage"}
	}
	next := cloneDiskState(s.state)
	next.PendingActivation = nil
	return s.commitLocked(ctx, next)
}

// BindArchive installs the first archive binding or performs a monotonic
// forward rebind. Forward rebind never inherits the previous ACK watermark.
func (s *Store) BindArchive(
	ctx context.Context,
	serverIdentityID string,
	generation uint64,
	archiveID string,
	coverageBeginsAt Cursor,
	now time.Time,
) (RebindResult, error) {
	presented := ArchiveBinding{
		ServerIdentityID: serverIdentityID,
		Generation:       generation,
		ArchiveID:        archiveID,
		CoverageBeginsAt: coverageBeginsAt,
	}
	if err := validateBinding(presented); err != nil {
		return RebindResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RebindResult{}, ErrClosed
	}
	if !s.ready {
		return RebindResult{}, ErrNotReady
	}
	next, result, err := applyArchiveBinding(s.state, presented, now)
	if err != nil {
		return RebindResult{}, err
	}
	if !result.Changed {
		return result, nil
	}
	if err := s.commitLocked(ctx, next); err != nil {
		return RebindResult{}, err
	}
	return result, nil
}

func applyArchiveBinding(state diskState, presented ArchiveBinding, now time.Time) (diskState, RebindResult, error) {
	next := cloneDiskState(state)
	if state.BoundArchive == nil {
		next.BoundArchive = cloneBinding(&presented)
		return next, RebindResult{Changed: true, Current: *cloneBinding(&presented)}, nil
	}
	bound := state.BoundArchive
	if bound.ServerIdentityID != presented.ServerIdentityID {
		return diskState{}, RebindResult{}, fmt.Errorf("%w: bound %q, presented %q",
			ErrServerIdentityMismatch, bound.ServerIdentityID, presented.ServerIdentityID)
	}
	if presented.Generation < bound.Generation {
		return diskState{}, RebindResult{}, &ArchiveRollbackError{
			BoundGeneration: bound.Generation, PresentedGeneration: presented.Generation,
		}
	}
	if presented.Generation == bound.Generation {
		if presented.ArchiveID != bound.ArchiveID {
			return diskState{}, RebindResult{}, &ArchiveInvariantError{
				Generation: presented.Generation, BoundArchiveID: bound.ArchiveID,
				PresentedArchiveID: presented.ArchiveID,
				Reason:             "same generation has a different archive_id",
			}
		}
		return next, RebindResult{Changed: false, Current: *cloneBinding(bound)}, nil
	}
	if presented.ArchiveID == bound.ArchiveID {
		return diskState{}, RebindResult{}, &ArchiveInvariantError{
			Generation: presented.Generation, BoundArchiveID: bound.ArchiveID,
			PresentedArchiveID: presented.ArchiveID,
			Reason:             "new generation reused the active archive_id",
		}
	}

	retired := RetiredArchive{
		Generation:   bound.Generation,
		ArchiveID:    bound.ArchiveID,
		AckedThrough: cloneCursor(bound.AckedThrough),
		RetiredAt:    timestamp(now),
	}
	next.RetiredArchives = append(next.RetiredArchives, retired)
	next.BoundArchive = cloneBinding(&presented)
	return next, RebindResult{
		Changed: true, Previous: cloneRetired(&retired), Current: *cloneBinding(&presented),
	}, nil
}

// AdvanceArchiveACK advances the inclusive watermark for the active archive.
// It rejects regression but treats an identical cursor as idempotent.
func (s *Store) AdvanceArchiveACK(ctx context.Context, archiveID string, cursor Cursor) error {
	if !validCursor(cursor) {
		return &StateInvariantError{Reason: "ACK cursor is invalid"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	if s.state.BoundArchive == nil {
		return &StateInvariantError{Reason: "cannot ACK without an archive binding"}
	}
	if archiveID != s.state.BoundArchive.ArchiveID {
		return &ArchiveInvariantError{
			Generation:         s.state.BoundArchive.Generation,
			BoundArchiveID:     s.state.BoundArchive.ArchiveID,
			PresentedArchiveID: archiveID,
			Reason:             "ACK targets a different archive_id",
		}
	}
	if compareCursor(cursor, s.state.BoundArchive.CoverageBeginsAt) < 0 {
		return fmt.Errorf("%w: ACK (%d,%d) precedes coverage start (%d,%d)",
			ErrCursorRollback,
			cursor.Incarnation, cursor.Seq,
			s.state.BoundArchive.CoverageBeginsAt.Incarnation,
			s.state.BoundArchive.CoverageBeginsAt.Seq,
		)
	}
	current := s.state.BoundArchive.AckedThrough
	if current != nil {
		comparison := compareCursor(cursor, *current)
		if comparison < 0 {
			return fmt.Errorf("%w: current (%d,%d), proposed (%d,%d)",
				ErrCursorRollback, current.Incarnation, current.Seq, cursor.Incarnation, cursor.Seq)
		}
		if comparison == 0 {
			return nil
		}
	}
	next := cloneDiskState(s.state)
	next.BoundArchive.AckedThrough = cloneCursor(&cursor)
	return s.commitLocked(ctx, next)
}

// AdvanceLastDockerEventAt persists a monotonic --since watermark only after
// the corresponding observed events are represented in the Audit WAL.
func (s *Store) AdvanceLastDockerEventAt(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		return &StateInvariantError{Reason: "Docker event timestamp is zero"}
	}
	at = at.UTC().Round(0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	if s.state.LastDockerEventAt != "" {
		current, _ := time.Parse(time.RFC3339Nano, s.state.LastDockerEventAt)
		if !at.After(current) {
			return nil
		}
	}
	next := cloneDiskState(s.state)
	next.LastDockerEventAt = timestamp(at)
	return s.commitLocked(ctx, next)
}

// SetDockerSnapshotSHA256 durably records only a bounded digest of the
// current container inventory. Docker state itself is never mirrored here.
func (s *Store) SetDockerSnapshotSHA256(ctx context.Context, digest string) error {
	if !validSHA256(digest) {
		return &StateInvariantError{Reason: "Docker snapshot SHA-256 is invalid"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	if s.state.DockerSnapshotSHA256 == digest {
		return nil
	}
	next := cloneDiskState(s.state)
	next.DockerSnapshotSHA256 = digest
	return s.commitLocked(ctx, next)
}

// GracefulClose records the caller-confirmed durable WAL sequence, fsyncs the
// state and directory, and releases the process lock. The caller must stop
// audit production and fsync its WAL before invoking this method.
func (s *Store) GracefulClose(ctx context.Context, lastDurableSeq uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.ready {
		return ErrNotReady
	}
	next := cloneDiskState(s.state)
	next.CleanClose = &CleanClose{
		Incarnation: next.CurrentIncarnation, LastDurableSeq: lastDurableSeq, ClosedAt: timestamp(now),
	}
	if err := s.commitLocked(ctx, next); err != nil {
		return err
	}
	return s.closeLocked()
}

// Close releases the process lock without writing clean_close. It represents
// an unclean process exit for the next Open.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.closeLocked()
}

func (s *Store) commitLocked(ctx context.Context, next diskState) error {
	if err := validateDiskState(next); err != nil {
		return err
	}
	if err := writeStateAtomic(ctx, s.dir, s.path, next, s.hooks); err != nil {
		// A rename may already have happened when directory fsync fails. Do not
		// permit more transitions from an uncertain in-memory base; the Agent
		// must stop and reopen/reconcile the durable file.
		s.ready = false
		return err
	}
	s.state = next
	return nil
}

func (s *Store) closeLocked() error {
	s.ready = false
	s.closed = true
	if s.lockFile == nil {
		return nil
	}
	lockFile := s.lockFile
	s.lockFile = nil
	return releaseLock(lockFile)
}

func compareCursor(left, right Cursor) int {
	if left.Incarnation < right.Incarnation {
		return -1
	}
	if left.Incarnation > right.Incarnation {
		return 1
	}
	if left.Seq < right.Seq {
		return -1
	}
	if left.Seq > right.Seq {
		return 1
	}
	return 0
}

func snapshotOf(state diskState) Snapshot {
	return Snapshot{
		AgentID: state.AgentID, Credential: cloneCredential(state.Credential),
		PendingActivation:  clonePendingActivation(state.PendingActivation),
		BoundArchive:       cloneBinding(state.BoundArchive),
		RetiredArchives:    cloneRetiredSlice(state.RetiredArchives),
		CurrentIncarnation: state.CurrentIncarnation, CleanClose: cloneCleanClose(state.CleanClose),
		LastDockerEventAt:    parseOptionalTimestamp(state.LastDockerEventAt),
		DockerSnapshotSHA256: state.DockerSnapshotSHA256,
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func parseOptionalTimestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func cloneDiskState(state diskState) diskState {
	state.Credential = cloneCredential(state.Credential)
	state.PendingActivation = clonePendingActivation(state.PendingActivation)
	state.BoundArchive = cloneBinding(state.BoundArchive)
	state.RetiredArchives = cloneRetiredSlice(state.RetiredArchives)
	state.CleanClose = cloneCleanClose(state.CleanClose)
	return state
}

func clonePendingActivation(value *PendingCredentialActivation) *PendingCredentialActivation {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Previous = cloneCredential(value.Previous)
	return &copy
}

func cloneCredential(value Credential) Credential {
	value.Data = append([]byte(nil), value.Data...)
	return value
}

func cloneCursor(value *Cursor) *Cursor {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBinding(value *ArchiveBinding) *ArchiveBinding {
	if value == nil {
		return nil
	}
	copy := *value
	copy.AckedThrough = cloneCursor(value.AckedThrough)
	return &copy
}

func cloneRetired(value *RetiredArchive) *RetiredArchive {
	if value == nil {
		return nil
	}
	copy := *value
	copy.AckedThrough = cloneCursor(value.AckedThrough)
	return &copy
}

func cloneRetiredSlice(values []RetiredArchive) []RetiredArchive {
	result := make([]RetiredArchive, len(values))
	for index := range values {
		result[index] = *cloneRetired(&values[index])
	}
	return result
}

func cloneCleanClose(value *CleanClose) *CleanClose {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
