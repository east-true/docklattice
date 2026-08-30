package agentops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/docklattice/internal/backup"
	"github.com/east-true/docklattice/internal/composeexec"
	"github.com/east-true/docklattice/internal/config"
	"github.com/east-true/docklattice/internal/diskbudget"
	"github.com/east-true/docklattice/internal/operation"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/safefile"
)

type fixedProjectCatalog struct{ project composeexec.Project }

func (catalog fixedProjectCatalog) Project(_ context.Context, uid string) (composeexec.Project, bool, error) {
	return catalog.project, uid == "project", nil
}
func (fixedProjectCatalog) FilesystemMutationAllowed(context.Context, string) (bool, string) {
	return true, ""
}
func (fixedProjectCatalog) ApprovedReadOnlyFiles(context.Context, string) ([]safefile.ApprovedFile, bool, error) {
	return nil, true, nil
}

type deniedFilesystem struct{}

func (deniedFilesystem) FilesystemMutationAllowed(context.Context, string) (bool, string) {
	return false, "READ_ONLY"
}

type captureCompose struct {
	mu      sync.Mutex
	specs   []composeexec.Spec
	started chan struct{}
	fail    bool
}

func (runner *captureCompose) Run(ctx context.Context, spec composeexec.Spec, _ chan<- composeexec.OutputChunk) (composeexec.Result, error) {
	runner.mu.Lock()
	runner.specs = append(runner.specs, spec)
	runner.mu.Unlock()
	if runner.started != nil {
		select {
		case <-runner.started:
		default:
			close(runner.started)
		}
		<-ctx.Done()
		return composeexec.Result{ExitCode: -1, Canceled: true}, ctx.Err()
	}
	if runner.fail {
		return composeexec.Result{ExitCode: 1}, nil
	}
	return composeexec.Result{ExitCode: 0}, nil
}

func (runner *captureCompose) lastSpec() composeexec.Spec {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.specs[len(runner.specs)-1]
}

func newMutationService(t *testing.T, project composeexec.Project, compose Compose, backups BackupManager, admit DiskAdmitter, timeouts config.OperationTimeouts) (*Service, *operation.Engine) {
	t.Helper()
	engine := operation.NewDefault()
	service, err := New(Config{
		Engine: engine, Docker: &fakeDocker{}, Compose: compose, Projects: fixedProjectCatalog{project}, Approvals: fixedProjectCatalog{project}, Filesystem: fixedProjectCatalog{project},
		Rescanner: fakeRescan{}, Backups: backups, Admission: admit, Timeouts: timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, engine
}

func allowAllDisk(context.Context, diskbudget.Operation) error { return nil }

type stagingAdmission struct {
	mu                 sync.Mutex
	total, free, bytes int64
	calls              int
	err                error
}

func (*stagingAdmission) AdmitOperation(context.Context, diskbudget.Operation) error { return nil }

func (admission *stagingAdmission) AdmitProjectStaging(_ context.Context, total, free, bytes int64) error {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	admission.calls++
	admission.total, admission.free, admission.bytes = total, free, bytes
	return admission.err
}

func TestReadOnlyProjectAllowsComposeButRejectsFileMutationBeforeAcceptance(t *testing.T) {
	project := composeexec.Project{WorkingDir: "/srv/project", Files: []string{"/srv/project/compose.yaml"}, Name: "project"}
	engine := operation.NewDefault()
	service, err := New(Config{
		Engine: engine, Docker: &fakeDocker{}, Compose: &captureCompose{}, Projects: fixedProjectCatalog{project}, Approvals: fixedProjectCatalog{project},
		Filesystem: deniedFilesystem{}, Rescanner: fakeRescan{}, Backups: fakeBackups{},
		Admission: DiskAdmitterFunc(allowAllDisk), Timeouts: config.V1Defaults().OperationTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := writePayload(t, strings.Repeat("0", 64), "services: {}\n")
	_, err = service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "read-only-write", ProjectKey: "project", Type: string(operation.TypeComposeFileWrite),
		Target: "compose.yaml", Payload: payload,
	})
	if !errors.Is(err, ErrProjectUnavailable) || !strings.Contains(err.Error(), "READ_ONLY") {
		t.Fatalf("read-only mutation error = %v", err)
	}
	if _, exists := engine.Get("read-only-write"); exists {
		t.Fatal("read-only mutation was durably accepted")
	}
}

func writePayload(t *testing.T, expected, content string) []byte {
	t.Helper()
	payload, err := json.Marshal(fileWritePayload{Version: 1, ExpectedSHA256: expected, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func sha(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestFileWriteUsesComposeValidationPreWriteSnapshotAndPayloadHashOnly(t *testing.T) {
	base := t.TempDir()
	workingDir := filepath.Join(base, "project")
	stateDir := filepath.Join(base, "state")
	if err := os.Mkdir(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "services:\n  api:\n    image: old\n"
	updated := "services:\n  api:\n    image: secret-image\n"
	composePath := filepath.Join(workingDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureCompose{}
	project := composeexec.Project{WorkingDir: workingDir, Files: []string{composePath}, Name: "project"}
	service, engine := newMutationService(t, project, runner, manager, DiskAdmitterFunc(allowAllDisk), config.V1Defaults().OperationTimeout)
	payload := writePayload(t, sha(original), updated)
	_, err = service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "write-compose", Type: string(operation.TypeComposeFileWrite), ProjectKey: "project",
		Target: "compose.yaml", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "write-compose")
	if record.Status != operation.StatusSuccess || record.Phase != operation.PhaseFinalizing || record.CommitStartedAt.IsZero() {
		t.Fatalf("record = %+v", record)
	}
	got, err := os.ReadFile(composePath)
	if err != nil || string(got) != updated {
		t.Fatalf("target = %q, %v", got, err)
	}
	metadata, err := manager.List(context.Background(), "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[0].Trigger != backup.TriggerPreWrite {
		t.Fatalf("pre-write backups = %+v", metadata)
	}
	spec := runner.lastSpec()
	if spec.Operation != composeexec.OperationConfig || spec.Flags.ConfigOutput != composeexec.ConfigOutputQuiet ||
		len(spec.Project.Files) != 1 || !strings.Contains(spec.Project.Files[0], ".docklattice-stage-") {
		t.Fatalf("validation spec = %+v", spec)
	}
	wantPayloadHash := sha(string(payload))
	serializedRecord := record.Result + record.Error + string(record.OutputTail)
	if record.PayloadHash != wantPayloadHash || record.Target != "compose.yaml" || strings.Contains(serializedRecord, "secret-image") || strings.Contains(serializedRecord, workingDir) {
		t.Fatalf("secret/path leaked or payload hash mismatch: %+v", record)
	}
	changedPayload := writePayload(t, sha(original), "services: {}\n")
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "write-compose", Type: string(operation.TypeComposeFileWrite), ProjectKey: "project",
		Target: "compose.yaml", Payload: changedPayload,
	}); err == nil || !operation.HasErrorCode(err, operation.CodeSpecMismatch) {
		t.Fatalf("changed secret-bearing payload idempotency error = %v", err)
	}
}

func TestEnvWriteValidatesWithStagedEnvFile(t *testing.T) {
	base := t.TempDir()
	workingDir, stateDir := filepath.Join(base, "project"), filepath.Join(base, "state")
	if err := os.Mkdir(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(workingDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte("TOKEN=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureCompose{}
	service, engine := newMutationService(t, composeexec.Project{WorkingDir: workingDir, Files: []string{composePath}, Name: "project"}, runner, manager, DiskAdmitterFunc(allowAllDisk), config.V1Defaults().OperationTimeout)
	payload := writePayload(t, sha("TOKEN=old\n"), "TOKEN=new-secret\n")
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "write-env", Type: string(operation.TypeEnvWrite), ProjectKey: "project", Target: ".env", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	if record := waitTerminal(t, engine, "write-env"); record.Status != operation.StatusSuccess || strings.Contains(record.Result, "new-secret") {
		t.Fatalf("record = %+v", record)
	}
	if envFile := runner.lastSpec().Project.EnvFile; !strings.Contains(envFile, ".docklattice-stage-") || !filepath.IsAbs(envFile) {
		t.Fatalf("validation env file = %q", envFile)
	}
}

func TestFileWriteTimeoutCancelsBeforeCommitWithoutSnapshotOrMutation(t *testing.T) {
	base := t.TempDir()
	workingDir, stateDir := filepath.Join(base, "project"), filepath.Join(base, "state")
	if err := os.Mkdir(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "services: {}\n"
	composePath := filepath.Join(workingDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureCompose{started: make(chan struct{})}
	timeouts := config.V1Defaults().OperationTimeout
	timeouts.FileWrite = 10 * time.Millisecond
	service, engine := newMutationService(t, composeexec.Project{WorkingDir: workingDir, Files: []string{composePath}, Name: "project"}, runner, manager, DiskAdmitterFunc(allowAllDisk), timeouts)
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "write-timeout", Type: string(operation.TypeComposeFileWrite), ProjectKey: "project", Target: "compose.yaml",
		Payload: writePayload(t, sha(original), "services:\n  api: {}\n"),
	}); err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "write-timeout")
	if record.Status != operation.StatusCanceled || record.CancelReason != operation.CancelReasonTimeout || !record.CommitStartedAt.IsZero() {
		t.Fatalf("record = %+v", record)
	}
	got, _ := os.ReadFile(composePath)
	metadata, listErr := manager.List(context.Background(), "project")
	if string(got) != original || listErr != nil || len(metadata) != 0 {
		t.Fatalf("post-timeout target=%q metadata=%+v err=%v", got, metadata, listErr)
	}
}

func TestFileWriteUsesOpenedProjectRootFilesystemAdmissionBeforeStaging(t *testing.T) {
	base := t.TempDir()
	workingDir, stateDir := filepath.Join(base, "project"), filepath.Join(base, "state")
	if err := os.Mkdir(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "services: {}\n"
	updated := "services:\n  api: {}\n"
	composePath := filepath.Join(workingDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("project filesystem pressure")
	admission := &stagingAdmission{err: denied}
	service, engine := newMutationService(t,
		composeexec.Project{WorkingDir: workingDir, Files: []string{composePath}, Name: "project"},
		&captureCompose{}, manager, admission, config.V1Defaults().OperationTimeout)
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "write-project-pressure", Type: string(operation.TypeComposeFileWrite), ProjectKey: "project", Target: "compose.yaml",
		Payload: writePayload(t, sha(original), updated),
	}); err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "write-project-pressure")
	if record.Status != operation.StatusFailed || !strings.Contains(record.Error, denied.Error()) {
		t.Fatalf("record = %+v", record)
	}
	admission.mu.Lock()
	calls, total, free, bytes := admission.calls, admission.total, admission.free, admission.bytes
	admission.mu.Unlock()
	if calls != 1 || total <= 0 || free <= 0 || free > total || bytes != int64(len(updated)) {
		t.Fatalf("project admission calls=%d total=%d free=%d bytes=%d", calls, total, free, bytes)
	}
	if got, err := os.ReadFile(composePath); err != nil || string(got) != original {
		t.Fatalf("target=%q err=%v", got, err)
	}
	if snapshots, err := manager.List(context.Background(), "project"); err != nil || len(snapshots) != 0 {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
}

func TestBackupCreateAndRestoreUseManagerJournalSnapshotsAndCommitGate(t *testing.T) {
	base := t.TempDir()
	workingDir, stateDir := filepath.Join(base, "project"), filepath.Join(base, "state")
	if err := os.Mkdir(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(workingDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("version-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(stateDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	project := composeexec.Project{WorkingDir: workingDir, Files: []string{composePath}, Name: "project"}
	service, engine := newMutationService(t, project, &captureCompose{}, manager, DiskAdmitterFunc(allowAllDisk), config.V1Defaults().OperationTimeout)
	createPayload, _ := json.Marshal(backupCreatePayload{Version: 1, RelativePaths: []string{"compose.yaml"}})
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "backup-create", Type: string(operation.TypeBackupCreate), ProjectKey: "project", Payload: createPayload,
	}); err != nil {
		t.Fatal(err)
	}
	if record := waitTerminal(t, engine, "backup-create"); record.Status != operation.StatusSuccess || !record.CommitStartedAt.IsZero() {
		t.Fatalf("create record = %+v", record)
	}
	metadata, err := manager.List(context.Background(), "project")
	if err != nil || len(metadata) != 1 || metadata[0].Trigger != backup.TriggerManual {
		t.Fatalf("manual metadata = %+v, %v", metadata, err)
	}
	backupID := metadata[0].BackupID
	if err := os.WriteFile(composePath, []byte("version-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "backup-restore", Type: string(operation.TypeBackupRestore), ProjectKey: "project", Target: backupID,
		Payload: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	restoreRecord := waitTerminal(t, engine, "backup-restore")
	if restoreRecord.Status != operation.StatusSuccess || restoreRecord.CommitStartedAt.IsZero() || restoreRecord.Revision < 7 {
		t.Fatalf("restore record = %+v", restoreRecord)
	}
	got, _ := os.ReadFile(composePath)
	metadata, err = manager.List(context.Background(), "project")
	if string(got) != "version-one\n" || err != nil || len(metadata) != 2 || metadata[0].Trigger != backup.TriggerPreRestore {
		t.Fatalf("restored=%q metadata=%+v err=%v", got, metadata, err)
	}
}

type postCommitRestore struct{ committed chan struct{} }

func (manager postCommitRestore) Create(context.Context, backup.CreateRequest) (backup.Backup, error) {
	return backup.Backup{}, nil
}
func (manager postCommitRestore) CheckChangeAllowed(string) error              { return nil }
func (manager postCommitRestore) PruneAutomatic(string, int) ([]string, error) { return nil, nil }
func (manager postCommitRestore) Restore(ctx context.Context, request backup.RestoreRequest) (backup.RestoreResult, error) {
	if err := request.CommitGate.EnterRestoreCommit(ctx); err != nil {
		return backup.RestoreResult{}, err
	}
	close(manager.committed)
	time.Sleep(40 * time.Millisecond)
	return backup.RestoreResult{RestoredFiles: 1, PreRestoreSnapshotID: "snapshot"}, nil
}

func TestRestoreTimeoutAfterCommitIsTooLateAndDoesNotForceCancel(t *testing.T) {
	committed := make(chan struct{})
	timeouts := config.V1Defaults().OperationTimeout
	timeouts.BackupRestore = 5 * time.Millisecond
	service, engine := newMutationService(t,
		composeexec.Project{WorkingDir: t.TempDir(), Files: []string{"/tmp/compose.yaml"}, Name: "project"},
		&captureCompose{}, postCommitRestore{committed: committed}, DiskAdmitterFunc(allowAllDisk), timeouts)
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, producttransport.OperationRequest{
		OperationID: "restore-too-late", Type: string(operation.TypeBackupRestore), ProjectKey: "project", Target: "backup-id",
		Payload: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	<-committed
	record := waitTerminal(t, engine, "restore-too-late")
	if record.Status != operation.StatusSuccess || record.Phase != operation.PhaseFinalizing || record.CommitStartedAt.IsZero() || record.CancelReason != "" {
		t.Fatalf("record = %+v", record)
	}
}

func TestMutationPayloadIsStrictAndDiskDenialCreatesRejectedMinimumRecord(t *testing.T) {
	project := composeexec.Project{WorkingDir: t.TempDir(), Files: []string{"/tmp/compose.yaml"}, Name: "project"}
	denied := errors.New("DEGRADED_STORAGE")
	service, engine := newMutationService(t, project, &captureCompose{}, fakeBackups{}, DiskAdmitterFunc(func(_ context.Context, kind diskbudget.Operation) error {
		if kind != diskbudget.OperationFileWrite {
			t.Fatalf("admission kind = %s", kind)
		}
		return denied
	}), config.V1Defaults().OperationTimeout)
	badRequests := []producttransport.OperationRequest{
		{OperationID: "absolute", Type: string(operation.TypeEnvWrite), ProjectKey: "project", Target: "/etc/passwd", Payload: []byte(`{"version":1,"expected_sha256":"` + strings.Repeat("a", 64) + `","content":"x"}`)},
		{OperationID: "unknown", Type: string(operation.TypeEnvWrite), ProjectKey: "project", Target: ".env", Payload: []byte(`{"version":1,"expected_sha256":"` + strings.Repeat("a", 64) + `","content":"x","extra":true}`)},
		{OperationID: "mismatch", Type: string(operation.TypeOverrideWrite), ProjectKey: "project", Target: ".env", Payload: []byte(`{"version":1,"expected_sha256":"` + strings.Repeat("a", 64) + `","content":"x"}`)},
		{OperationID: "duplicate", Type: string(operation.TypeEnvWrite), ProjectKey: "project", Target: ".env", Payload: []byte(`{"version":1,"expected_sha256":"` + strings.Repeat("a", 64) + `","content":"x","content":"y"}`)},
	}
	for _, request := range badRequests {
		if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err == nil {
			t.Fatalf("unsafe request succeeded: %+v", request)
		}
		if _, exists := engine.Get(request.OperationID); exists {
			t.Fatalf("unsafe request created operation %q", request.OperationID)
		}
	}
	request := producttransport.OperationRequest{
		OperationID: "denied", Type: string(operation.TypeEnvWrite), ProjectKey: "project", Target: ".env",
		Payload: writePayload(t, strings.Repeat("a", 64), "secret-value"),
	}
	if _, err := service.StartOperation(context.Background(), producttransport.SessionInfo{}, request); err != nil {
		t.Fatal(err)
	}
	record := waitTerminal(t, engine, "denied")
	if record.Status != operation.StatusRejected || !strings.Contains(record.Error, denied.Error()) || strings.Contains(record.Error, "secret-value") || record.PayloadHash != sha(string(request.Payload)) {
		t.Fatalf("rejected record = %+v", record)
	}
}
