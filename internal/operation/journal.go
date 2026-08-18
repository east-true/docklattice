package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type PersistenceClass string

const (
	PersistenceMinimal PersistenceClass = "OPERATION_MINIMUM"
	PersistenceOutput  PersistenceClass = "OPERATION_OUTPUT"
)

var ErrOutputPersistenceDropped = errors.New("operation output persistence dropped")

type PersistenceAdmission struct {
	Class          PersistenceClass
	EstimatedBytes int64
}

// PersistenceAdmitter is the disk-budget/reserve boundary. Policy remains in
// the caller; this package only distinguishes required minimal state from an
// optional output tail.
type PersistenceAdmitter interface {
	AdmitOperationPersistence(context.Context, PersistenceAdmission) error
}

// Journal persists one latest record per operation. Minimal writes must be
// durable; output writes may return ErrOutputPersistenceDropped.
type Journal interface {
	Load() ([]Record, error)
	Save(Record, bool) error
	Delete(string) error
}

// DiskPressureJournal is the concrete FileJournal cleanup boundary used by
// the Agent-wide eviction executor. Implementations must serialize these
// methods with Save/Load/Delete and report logical bytes actually unlinked.
type DiskPressureJournal interface {
	ReclaimAbandonedTempForDiskPressure(context.Context, int64) (int64, error)
	ReclaimOperationForDiskPressure(context.Context, string) (freedBytes int64, recordDeleted bool, err error)
}

type FileJournal struct {
	directory string
	admitter  PersistenceAdmitter
	mu        sync.Mutex
}

func NewFileJournal(agentStateDir string, admitter PersistenceAdmitter) (*FileJournal, error) {
	abs, err := filepath.Abs(agentStateDir)
	if err != nil {
		return nil, err
	}
	if err := ensureJournalDirectory(abs); err != nil {
		return nil, err
	}
	directory := filepath.Join(abs, "operations")
	if err := ensureJournalDirectory(directory); err != nil {
		return nil, err
	}
	return &FileJournal{directory: directory, admitter: admitter}, nil
}

func ensureJournalDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("operation: journal directory must be a non-symlink directory with mode 0700")
	}
	return nil
}

func (journal *FileJournal) Save(record Record, includeOutput bool) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	class := PersistenceMinimal
	var payload []byte
	var err error
	if includeOutput {
		class = PersistenceOutput
		payload, err = json.Marshal(persistedOutput{OperationID: record.OperationID, Tail: record.OutputTail, Truncated: record.OutputTruncated})
	} else {
		persisted := record
		persisted.StalledWarning = false
		persisted.OutputTail = nil
		payload, err = json.Marshal(persisted)
	}
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if journal.admitter != nil {
		if err := journal.admitter.AdmitOperationPersistence(context.Background(), PersistenceAdmission{Class: class, EstimatedBytes: int64(len(payload))}); err != nil {
			if includeOutput {
				return errors.Join(ErrOutputPersistenceDropped, err)
			}
			return err
		}
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	name := operationJournalName(record.OperationID)
	if includeOutput {
		name = operationOutputName(record.OperationID)
	}
	return writeJournalPayload(journal.directory, name, payload)
}

type persistedOutput struct {
	OperationID string `json:"operation_id"`
	Tail        []byte `json:"tail"`
	Truncated   bool   `json:"truncated"`
}

func writeJournalPayload(directory, name string, payload []byte) error {
	temporary := filepath.Join(directory, "."+name+".tmp")
	final := filepath.Join(directory, name)
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, final); err != nil {
		os.Remove(temporary)
		return err
	}
	return syncJournalDirectory(directory)
}

func (journal *FileJournal) Load() ([]Record, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entries, err := os.ReadDir(journal.directory)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(journal.directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, fmt.Errorf("operation: unsafe journal file %q", entry.Name())
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		after, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, after) {
			file.Close()
			return nil, fmt.Errorf("operation: journal changed while opening")
		}
		decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
		decoder.DisallowUnknownFields()
		var record Record
		decodeErr := decoder.Decode(&record)
		var trailing any
		trailingErr := decoder.Decode(&trailing)
		closeErr := file.Close()
		if decodeErr != nil || !errors.Is(trailingErr, io.EOF) || closeErr != nil {
			return nil, fmt.Errorf("operation: invalid journal %q", entry.Name())
		}
		if operationJournalName(record.OperationID) != entry.Name() {
			return nil, fmt.Errorf("operation: journal identity mismatch")
		}
		if err := validateRecord(record); err != nil {
			return nil, err
		}
		outputPath := filepath.Join(journal.directory, operationOutputName(record.OperationID))
		if output, outputErr := loadPersistedOutput(outputPath, record.OperationID); outputErr == nil {
			record.OutputTail = output.Tail
			record.OutputTruncated = output.Truncated
		} else if !errors.Is(outputErr, os.ErrNotExist) {
			return nil, outputErr
		}
		result = append(result, record)
	}
	return result, nil
}

func (journal *FileJournal) Delete(operationID string) error {
	_, _, err := journal.ReclaimOperationForDiskPressure(context.Background(), operationID)
	return err
}

// ReclaimAbandonedTempForDiskPressure deletes only interrupted same-directory
// journal writes. The journal mutex proves no Save currently owns such a file.
func (journal *FileJournal) ReclaimAbandonedTempForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	if bytesNeeded <= 0 {
		return 0, nil
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entries, err := os.ReadDir(journal.directory)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, entry := range entries {
		if freed >= bytesNeeded {
			break
		}
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		path := filepath.Join(journal.directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return freed, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return freed, fmt.Errorf("operation: unsafe abandoned journal temp %q", name)
		}
		if err := os.Remove(path); err != nil {
			return freed, err
		}
		freed += info.Size()
	}
	if freed > 0 {
		if err := syncJournalDirectory(journal.directory); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

// ReclaimOperationForDiskPressure removes the optional output first and the
// minimal terminal record last. A failure can therefore truncate output while
// retaining the authoritative result, never the reverse. recordDeleted tells
// Engine how to keep its in-memory index consistent after an fsync failure.
func (journal *FileJournal) ReclaimOperationForDiskPressure(ctx context.Context, operationID string) (int64, bool, error) {
	if operationID == "" {
		return 0, false, &Error{Code: CodeInvalidSpec, Message: "operation ID is required"}
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	var freed int64
	remove := func(name string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		path := filepath.Join(journal.directory, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return false, fmt.Errorf("operation: unsafe reclaim candidate %q", name)
		}
		if err := os.Remove(path); err != nil {
			return false, err
		}
		freed += info.Size()
		return true, nil
	}
	if _, err := remove(operationOutputName(operationID)); err != nil {
		return freed, false, err
	}
	recordDeleted, err := remove(operationJournalName(operationID))
	if err != nil {
		return freed, false, err
	}
	if freed > 0 {
		if err := syncJournalDirectory(journal.directory); err != nil {
			return freed, recordDeleted, err
		}
	}
	return freed, recordDeleted, nil
}

func operationJournalName(operationID string) string {
	digest := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(digest[:]) + ".json"
}

func operationOutputName(operationID string) string {
	digest := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(digest[:]) + ".output"
}

func loadPersistedOutput(path, operationID string) (persistedOutput, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return persistedOutput{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return persistedOutput{}, fmt.Errorf("operation: unsafe output journal")
	}
	file, err := os.Open(path)
	if err != nil {
		return persistedOutput{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) {
		return persistedOutput{}, fmt.Errorf("operation: output journal changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var output persistedOutput
	if err := decoder.Decode(&output); err != nil {
		return persistedOutput{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || output.OperationID != operationID {
		return persistedOutput{}, fmt.Errorf("operation: invalid output journal")
	}
	return output, nil
}

func syncJournalDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateRecord(record Record) error {
	if record.OperationID == "" || !record.Type.Valid() || record.Revision == 0 || record.RequestedAt.IsZero() ||
		!validPayloadHash(record.PayloadHash) || !validStatus(record.Status) || !validPhase(record.Phase) || record.CancelMode != cancelModeForType(record.Type) || record.LastProgressAt.IsZero() {
		return &Error{Code: CodeInvalidSpec, Message: "invalid operation journal record"}
	}
	if requiresProjectLock(record.Type) && record.ProjectKey == "" {
		return &Error{Code: CodeInvalidSpec, Message: "journal record is missing project key"}
	}
	if record.Status.Terminal() != !record.FinishedAt.IsZero() {
		return &Error{Code: CodeInvalidSpec, Message: "journal terminal timestamp mismatch"}
	}
	if record.ManagedAuditDelivery != ManagedAuditNone && record.ManagedAuditDelivery != ManagedAuditPending && record.ManagedAuditDelivery != ManagedAuditDelivered {
		return &Error{Code: CodeInvalidSpec, Message: "invalid managed audit delivery state"}
	}
	if !record.Status.Terminal() && record.ManagedAuditDelivery != ManagedAuditNone {
		return &Error{Code: CodeInvalidSpec, Message: "nonterminal operation has managed audit delivery state"}
	}
	return nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusRequested, StatusDispatched, StatusRunning, StatusSuccess, StatusFailed, StatusCanceled, StatusInterrupted, StatusUnknown, StatusRejected:
		return true
	default:
		return false
	}
}

func validPhase(phase Phase) bool {
	return phase == PhasePreparing || phase == PhaseExecuting || phase == PhaseCommitting || phase == PhaseFinalizing
}

func (e *Engine) persist(record Record, includeOutput bool) error {
	if e.config.Journal == nil {
		return nil
	}
	return e.config.Journal.Save(record, includeOutput)
}

func (e *Engine) recoverJournal() error {
	if e.config.Journal == nil {
		return nil
	}
	records, err := e.config.Journal.Load()
	if err != nil {
		return err
	}
	now := e.config.Clock.Now()
	for _, record := range records {
		if _, duplicate := e.items[record.OperationID]; duplicate {
			return &Error{Code: CodeInvalidSpec, Message: "duplicate operation journal record"}
		}
		if len(record.OutputTail) > e.config.OutputTailBytes {
			record.OutputTail = append([]byte(nil), record.OutputTail[len(record.OutputTail)-e.config.OutputTailBytes:]...)
			record.OutputTruncated = true
		}
		operation := &Operation{engine: e, spec: Spec{OperationID: record.OperationID, ProjectKey: record.ProjectKey, Target: record.Target, Type: record.Type, PayloadHash: record.PayloadHash}, record: record, outputLimit: e.config.OutputTailBytes}
		if !record.Status.Terminal() {
			operation.record.Status = StatusInterrupted
			operation.record.PartialEffectsPossible = true
			operation.record.Error = "agent restarted while operation was nonterminal"
			operation.record.FinishedAt = now
			operation.record.LastProgressAt = now
			operation.record.Revision++
			if e.config.TerminalAuditor != nil {
				operation.record.ManagedAuditDelivery = ManagedAuditPending
			}
			operation.terminalNotified = true
			if err := e.persist(operation.record, false); err != nil {
				return err
			}
		} else {
			// Empty is a pre-outbox journal record. Once a TerminalAuditor is
			// configured, migrate it to PENDING before any delivery attempt.
			if e.config.TerminalAuditor != nil && operation.record.ManagedAuditDelivery == ManagedAuditNone {
				operation.record.ManagedAuditDelivery = ManagedAuditPending
				if err := e.persist(operation.record, false); err != nil {
					return err
				}
			}
			operation.terminalNotified = true
		}
		e.items[record.OperationID] = operation
		e.results = append(e.results, resultEntry{id: record.OperationID, finishedAt: operation.record.FinishedAt})
	}
	sort.Slice(e.results, func(i, j int) bool { return e.results[i].finishedAt.Before(e.results[j].finishedAt) })
	e.cleanupLocked(now)
	return nil
}
