package agentruntime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/agentstate"
	"github.com/east-true/dockpilot/internal/producttransport"
)

func TestAgentDiagnosticsDescribeLifecycleWithoutExposingSecrets(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()

	var output bytes.Buffer
	config.Diagnostics = &output
	var cancel context.CancelFunc
	config.Connect = func(
		context.Context,
		[]byte,
		uint64,
		producttransport.AgentHandler,
	) (producttransport.Session, error) {
		cancel()
		return nil, errors.New("dial refused credential=private-value\nforged_event=true")
	}

	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	maintainCtx, stop := context.WithCancel(context.Background())
	cancel = stop
	if err := runtime.Maintain(maintainCtx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	diagnostics := output.String()
	for _, event := range []string{
		"boot_started",
		"registration_started",
		"registration_complete",
		"docker_ready",
		"boot_ready",
		"connection_maintenance_started",
		"connection_failed",
		"connection_maintenance_stopped",
		"shutdown_started",
		"shutdown_complete",
	} {
		if !strings.Contains(diagnostics, "event="+event) {
			t.Errorf("diagnostics missing %q:\n%s", event, diagnostics)
		}
	}
	if strings.Contains(diagnostics, "private-value") {
		t.Fatalf("diagnostics exposed credential material:\n%s", diagnostics)
	}
	if !strings.Contains(diagnostics, "credential=[REDACTED]") {
		t.Fatalf("diagnostics did not mark redaction:\n%s", diagnostics)
	}
	if strings.Contains(diagnostics, "\nforged_event=true") {
		t.Fatalf("diagnostic error injected a second line:\n%s", diagnostics)
	}
}

func TestAgentDiagnosticsReportArchiveRollbackLocally(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()

	var output bytes.Buffer
	config.Diagnostics = &output
	runtime, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())

	serverIdentityID := runtime.credential.ServerIdentityID
	if _, err := runtime.bindAnnouncedArchive(context.Background(), producttransport.AuditArchiveDescriptor{
		ServerIdentityID: serverIdentityID,
		Generation:       2,
		AuditArchiveID:   "archive-2",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.bindAnnouncedArchive(context.Background(), producttransport.AuditArchiveDescriptor{
		ServerIdentityID: serverIdentityID,
		Generation:       1,
		AuditArchiveID:   "archive-1",
	})
	if !errors.Is(err, agentstate.ErrArchiveRollbackDetected) {
		t.Fatalf("rollback error = %v", err)
	}

	diagnostics := output.String()
	for _, expected := range []string{
		"event=audit_archive_rebound",
		"generation=\"2\"",
		"event=audit_archive_refused",
		"presented_generation=\"1\"",
		"bound_generation=\"2\"",
		"ARCHIVE_ROLLBACK_DETECTED",
	} {
		if !strings.Contains(diagnostics, expected) {
			t.Errorf("diagnostics missing %q:\n%s", expected, diagnostics)
		}
	}
}

func TestAgentDiagnosticLinesAreBoundedAndValidUTF8(t *testing.T) {
	var output bytes.Buffer
	diagnostics := newAgentDiagnostics(&output, nil)
	diagnostics.failure("oversized", errors.New(strings.Repeat("한", 2000)))

	line := output.String()
	if len(line) > agentDiagnosticLineLimit {
		t.Fatalf("diagnostic line length = %d", len(line))
	}
	if !utf8.ValidString(line) {
		t.Fatal("bounded diagnostic line is not valid UTF-8")
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "...\n") {
		t.Fatalf("bounded diagnostic line = %q", line)
	}
}
