package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testBackend struct {
	dashboard         Dashboard
	containers        []HostContainer
	images            []HostImage
	networks          []HostNetwork
	volumes           []HostVolume
	inventoryID       string
	auditPage         AuditPage
	auditRequest      AuditPageRequest
	auditAgent        string
	activityUID       string
	env               []EnvironmentEntry
	composeUID        string
	composeQuery      ComposeQuery
	compose           ComposeOutput
	projectLogUID     string
	projectLogRequest ProjectLogRequest
	file              ProjectFile
	backups           []Backup
	op                Operation
	err               error
	request           OperationRequest
	getAgent          string
	getID             string
	cancelAgent       string
	cancelID          string
	cancellation      OperationCancellation
	fileRequest       FileWriteRequest
	create            BackupCreateRequest
	restore           BackupRestoreRequest
	liveRequest       LiveRequest
	logStream         LogStream
	statsStream       StatsStream
}

func (b *testBackend) Dashboard(context.Context) (Dashboard, error) { return b.dashboard, b.err }
func (b *testBackend) Host(context.Context, string) (Host, error) {
	if b.err != nil {
		return Host{}, b.err
	}
	return b.dashboard.Hosts[0], nil
}
func (b *testBackend) HostContainers(_ context.Context, agentID string) ([]HostContainer, error) {
	b.inventoryID = agentID
	return append([]HostContainer(nil), b.containers...), b.err
}
func (b *testBackend) HostImages(_ context.Context, agentID string) ([]HostImage, error) {
	b.inventoryID = agentID
	return append([]HostImage(nil), b.images...), b.err
}
func (b *testBackend) HostNetworks(_ context.Context, agentID string) ([]HostNetwork, error) {
	b.inventoryID = agentID
	return append([]HostNetwork(nil), b.networks...), b.err
}
func (b *testBackend) HostVolumes(_ context.Context, agentID string) ([]HostVolume, error) {
	b.inventoryID = agentID
	return append([]HostVolume(nil), b.volumes...), b.err
}
func (b *testBackend) HostAudit(_ context.Context, agentID string, request AuditPageRequest) (AuditPage, error) {
	b.auditAgent, b.auditRequest = agentID, request
	return b.auditPage, b.err
}
func (b *testBackend) ProjectActivity(_ context.Context, projectUID string, request AuditPageRequest) (AuditPage, error) {
	b.activityUID, b.auditRequest = projectUID, request
	return b.auditPage, b.err
}
func (b *testBackend) ProjectEnvironment(context.Context, string) ([]EnvironmentEntry, error) {
	result := append([]EnvironmentEntry(nil), b.env...)
	return result, b.err
}
func (b *testBackend) ProjectComposePS(_ context.Context, projectUID string, request ComposeQuery) (ComposeOutput, error) {
	b.composeUID, b.composeQuery = projectUID, request
	return b.compose, b.err
}
func (b *testBackend) ProjectComposeConfig(_ context.Context, projectUID string, request ComposeQuery) (ComposeOutput, error) {
	b.composeUID, b.composeQuery = projectUID, request
	return b.compose, b.err
}
func (b *testBackend) OpenProjectLogs(_ context.Context, projectUID string, request ProjectLogRequest) (LogStream, error) {
	b.projectLogUID, b.projectLogRequest = projectUID, request
	return b.logStream, b.err
}
func (b *testBackend) ProjectFile(context.Context, string, string) (ProjectFile, error) {
	return b.file, b.err
}
func (b *testBackend) WriteProjectFile(_ context.Context, request FileWriteRequest) (Operation, error) {
	b.fileRequest = request
	return b.op, b.err
}
func (b *testBackend) ProjectBackups(context.Context, string) ([]Backup, error) {
	return append([]Backup(nil), b.backups...), b.err
}
func (b *testBackend) CreateBackup(_ context.Context, request BackupCreateRequest) (Operation, error) {
	b.create = request
	return b.op, b.err
}
func (b *testBackend) RestoreBackup(_ context.Context, request BackupRestoreRequest) (Operation, error) {
	b.restore = request
	return b.op, b.err
}
func (b *testBackend) StartOperation(_ context.Context, request OperationRequest) (Operation, error) {
	b.request = request
	return b.op, b.err
}
func (b *testBackend) GetOperation(_ context.Context, agentID, operationID string) (Operation, error) {
	b.getAgent, b.getID = agentID, operationID
	return b.op, b.err
}
func (b *testBackend) CancelOperation(_ context.Context, agentID, operationID string) (OperationCancellation, error) {
	b.cancelAgent, b.cancelID = agentID, operationID
	return b.cancellation, b.err
}
func (b *testBackend) OpenLogs(_ context.Context, request LiveRequest) (LogStream, error) {
	b.liveRequest = request
	return b.logStream, b.err
}
func (b *testBackend) OpenStats(_ context.Context, request LiveRequest) (StatsStream, error) {
	b.liveRequest = request
	return b.statsStream, b.err
}

type sliceLogStream struct {
	events []LogEvent
	closed bool
}

func (s *sliceLogStream) Recv(context.Context) (LogEvent, error) {
	if len(s.events) == 0 {
		return LogEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *sliceLogStream) Close() error { s.closed = true; return nil }

type sliceStatsStream struct {
	samples []StatsSample
	closed  bool
}

func (s *sliceStatsStream) Recv(context.Context) (StatsSample, error) {
	if len(s.samples) == 0 {
		return StatsSample{}, io.EOF
	}
	sample := s.samples[0]
	s.samples = s.samples[1:]
	return sample, nil
}
func (s *sliceStatsStream) Close() error { s.closed = true; return nil }

func newTestHandler(t *testing.T, backend *testBackend) *Handler {
	t.Helper()
	handler, err := New(backend)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestDashboardAndEmbeddedClient(t *testing.T) {
	backend := &testBackend{dashboard: Dashboard{Hosts: []Host{{
		ID: "agent-1", DisplayName: "host-a", State: "ACTIVE",
		Capabilities: Capabilities{Docker: Capability{Enabled: false, Reason: "socket unavailable"}},
	}}}}
	handler := newTestHandler(t, backend)
	for _, path := range []string{"/", "/hosts/agent-1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Dockpilot") ||
			!strings.Contains(response.Body.String(), `id="file-form"`) || !strings.Contains(response.Body.String(), `id="backup-list-form"`) ||
			!strings.Contains(response.Body.String(), `id="inventory-form"`) || !strings.Contains(response.Body.String(), `id="audit-form"`) || !strings.Contains(response.Body.String(), `id="project-logs-form"`) ||
			!strings.Contains(response.Body.String(), `id="activity-form"`) {
			t.Fatalf("GET %s = %d %q", path, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "object-src 'none'") {
			t.Fatalf("CSP = %q", got)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "socket unavailable") {
		t.Fatalf("dashboard = %d %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestTypedHostInventoryRoutesAreStrictAndCurated(t *testing.T) {
	id := strings.Repeat("a", 64)
	backend := &testBackend{
		containers: []HostContainer{{ID: id, Names: []string{"/app"}, Image: "repo/app:latest", State: "running", Status: "Up"}},
		images:     []HostImage{{ID: id, RepoTags: []string{"repo/app:latest"}, Containers: 1}},
		networks:   []HostNetwork{{ID: id, Name: "bridge", Driver: "bridge", Scope: "local"}},
		volumes:    []HostVolume{{Name: "data", Driver: "local", Scope: "local"}},
	}
	handler := newTestHandler(t, backend)
	for _, resource := range []string{"containers", "images", "networks", "volumes"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/hosts/agent-a/"+resource, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || backend.inventoryID != "agent-a" || response.Header().Get("Cache-Control") != "no-store" ||
			strings.Contains(body, "labels") || strings.Contains(body, "options") || strings.Contains(body, "mountpoint") {
			t.Fatalf("GET %s = %d %q id=%q", resource, response.Code, body, backend.inventoryID)
		}
	}

	for _, test := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/hosts/agent-a/images?all=true", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/images", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/hosts/agent-a/images", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/hosts/agent-a/volumes", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/hosts/agent-a/images/extra", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts//images", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		var body io.Reader
		if test.method == http.MethodGet && test.path == "/api/v1/hosts/agent-a/images" {
			body = strings.NewReader(`{"kind":"volume.list"}`)
		}
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, body))
		if response.Code != test.want {
			t.Errorf("%s %s = %d %q, want %d", test.method, test.path, response.Code, response.Body.String(), test.want)
		}
	}

	backend.err = ErrUnavailable
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/hosts/agent-a/networks", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "CAPABILITY_UNAVAILABLE") {
		t.Fatalf("unavailable response = %d %q", response.Code, response.Body.String())
	}
}

func TestEmbeddedInventoryClientIsExplicitAndFailClosed(t *testing.T) {
	handler := newTestHandler(t, &testBackend{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	body := response.Body.String()
	for _, marker := range []string{
		`#inventory-form`, `loadInventoryResource`, `inventoryController`,
		`Unavailable — no inventory shown.`, `loadHistoryPage`, `MAX_RENDERED_AUDIT_EVENTS`,
		`Stored canonical history remains available`, `Coverage is unknown`, `textContent`,
	} {
		if response.Code != http.StatusOK || !strings.Contains(body, marker) {
			t.Fatalf("embedded inventory client missing %q: status=%d", marker, response.Code)
		}
	}
	// The inventory functions are registered as event handlers. Loading the
	// dashboard itself must not contain a direct inventory fetch call.
	loadStart := strings.Index(body, "async function load()")
	if loadStart < 0 {
		t.Fatal("dashboard load function is missing")
	}
	loadEnd := strings.Index(body[loadStart:], "\n}\n\ndocument.querySelector(\"#inventory-agent\")")
	if loadEnd < 0 || strings.Contains(body[loadStart:loadStart+loadEnd], "loadInventoryResource(") {
		t.Fatal("dashboard load implicitly triggers host inventory")
	}
}

func TestTypedAuditRoutesScopeAndParseStableCursor(t *testing.T) {
	backend := &testBackend{auditPage: AuditPage{
		AgentID: "agent-a", Events: []AuditEvent{{
			Cursor: AuditCursor{Incarnation: 2, Seq: 3}, Kind: "OBSERVED", ResourceType: "container", ResourceID: "container-a", Action: "start", Count: 1,
		}},
		Coverage: AuditCoverage{Established: true, Gaps: []AuditCoverageGap{}, UnknownIncarnations: []uint64{}},
	}}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/hosts/agent-a/audit?limit=2&cursor=1:9", nil))
	if response.Code != http.StatusOK || backend.auditAgent != "agent-a" || backend.auditRequest.Limit != 2 ||
		backend.auditRequest.Cursor == nil || *backend.auditRequest.Cursor != (AuditCursor{Incarnation: 1, Seq: 9}) ||
		response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "metadata") || strings.Contains(response.Body.String(), "attributes") {
		t.Fatalf("host audit = %d %q scope=%q request=%+v", response.Code, response.Body.String(), backend.auditAgent, backend.auditRequest)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/activity", nil))
	if response.Code != http.StatusOK || backend.activityUID != "project-a" || backend.auditRequest.Limit != DefaultAuditPageSize || backend.auditRequest.Cursor != nil {
		t.Fatalf("project activity = %d %q scope=%q request=%+v", response.Code, response.Body.String(), backend.activityUID, backend.auditRequest)
	}

	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?unknown=x", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?limit=1&limit=2", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?limit=0", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?limit=501", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?limit=01", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?cursor=1", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/hosts/agent-a/audit?cursor=1:0", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/activity", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/hosts/agent-a/audit", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/projects/project-a/activity", "", http.StatusMethodNotAllowed},
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if response.Code != test.want {
			t.Errorf("%s %s = %d %q, want %d", test.method, test.path, response.Code, response.Body.String(), test.want)
		}
	}
}

func TestEnvironmentMasksSecretsUnlessExplicitlyRevealed(t *testing.T) {
	backend := &testBackend{env: []EnvironmentEntry{
		{Name: "PUBLIC", Value: "visible", Secret: true},
		{Name: "PASSWORD", Value: "correct horse", Secret: true},
	}}
	handler := newTestHandler(t, backend)
	for _, tt := range []struct {
		query, want string
		reject      string
	}{
		{"", "********", "correct horse"},
		{"?reveal=true", "correct horse", "********"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/environment"+tt.query, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tt.want) || strings.Contains(response.Body.String(), tt.reject) {
			t.Fatalf("query %q = %d %q", tt.query, response.Code, response.Body.String())
		}
	}
}

func TestProjectComposeQueryRoutesAreStrict(t *testing.T) {
	backend := &testBackend{compose: ComposeOutput{Output: "NAME STATE\nweb running\n"}}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/compose/ps?service=web&all=true", nil))
	if response.Code != http.StatusOK || backend.composeUID != "project-a" || !backend.composeQuery.All ||
		len(backend.composeQuery.Services) != 1 || backend.composeQuery.Services[0] != "web" ||
		!strings.Contains(response.Body.String(), "web running") {
		t.Fatalf("compose ps = %d %q request=%+v", response.Code, response.Body.String(), backend.composeQuery)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/compose/config?service=web", nil))
	if response.Code != http.StatusOK || backend.composeUID != "project-a" || backend.composeQuery.All {
		t.Fatalf("compose config = %d %q request=%+v", response.Code, response.Body.String(), backend.composeQuery)
	}
	for _, test := range []struct {
		method, path string
		body         io.Reader
		want         int
	}{
		{http.MethodGet, "/api/v1/projects/project-a/compose/ps?all=1", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/config?all=true", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/ps?service=web&service=web", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/ps?service=--help", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/ps", strings.NewReader("{}"), http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/project-a/compose/ps", nil, http.StatusMethodNotAllowed},
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, test.body))
		if response.Code != test.want {
			t.Fatalf("%s %s = %d %q", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestProjectComposeLogRouteUsesOnlyProjectServiceTarget(t *testing.T) {
	stream := &sliceLogStream{events: []LogEvent{{Data: []byte("web | ready\n"), Stream: "STDOUT"}, {Terminal: true}}}
	backend := &testBackend{logStream: stream}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/compose/logs?service=web&service=worker&tail=100&follow=true&timestamps=true", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		backend.projectLogUID != "project-a" || !backend.projectLogRequest.Follow || backend.projectLogRequest.TailLines != 100 || !backend.projectLogRequest.Timestamps ||
		len(backend.projectLogRequest.Services) != 2 || backend.projectLogRequest.Services[0] != "web" || backend.projectLogRequest.Services[1] != "worker" ||
		!stream.closed || !strings.Contains(response.Body.String(), "event: log") || !strings.Contains(response.Body.String(), "d2ViIHwgcmVhZHkK") {
		t.Fatalf("project Compose logs = %d %q request=%+v", response.Code, response.Body.String(), backend.projectLogRequest)
	}
	for _, test := range []struct {
		method, path string
		body         io.Reader
		lastEventID  string
		want         int
	}{
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?agent_id=agent", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?container_id=abc", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?service=web&service=web", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?service=--help", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?tail=010", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs?tail=%2B1", nil, "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs", strings.NewReader("{}"), "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/projects/project-a/compose/logs", nil, "1", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/project-a/compose/logs", nil, "", http.StatusMethodNotAllowed},
	} {
		response = httptest.NewRecorder()
		request := httptest.NewRequest(test.method, test.path, test.body)
		if test.lastEventID != "" {
			request.Header.Set("Last-Event-ID", test.lastEventID)
		}
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s %s = %d %q", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestProjectFileRoutesMaskSecretsAndDispatchTypedWrite(t *testing.T) {
	secret := "http-file-secret"
	backend := &testBackend{
		file: ProjectFile{RelativePath: ".env", Content: secret, SHA256: strings.Repeat("a", 64), Secret: true},
		op:   Operation{ID: "write-1", Status: "ACCEPTED"},
	}
	handler := newTestHandler(t, backend)
	for _, test := range []struct {
		query, want, reject string
	}{
		{"?path=.env", "********", secret},
		{"?path=.env&reveal=true", secret, "********"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/files"+test.query, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) || strings.Contains(response.Body.String(), test.reject) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("file query %q = %d %q", test.query, response.Code, response.Body.String())
		}
	}

	body := `{"operation_id":"write-1","relative_path":".env","expected_sha256":"` + strings.Repeat("a", 64) + `","content":"TOKEN=` + secret + `"}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-a/files", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || backend.fileRequest.ProjectUID != "project-a" || backend.fileRequest.RelativePath != ".env" || backend.fileRequest.Content != "TOKEN="+secret {
		t.Fatalf("file write = %d %q request=%+v", response.Code, response.Body.String(), backend.fileRequest)
	}

	bad := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-a/files", strings.NewReader(`{"operation_id":"write-2","relative_path":".env","expected_sha256":"x","content":"`+secret+`","unknown":true}`))
	bad.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, bad)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("strict file write = %d %q", response.Code, response.Body.String())
	}
	duplicate := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-a/files", strings.NewReader(`{"operation_id":"write-2","operation_id":"write-3","relative_path":".env","expected_sha256":"x","content":"`+secret+`"}`))
	duplicate.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, duplicate)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "duplicate field") || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("duplicate file write = %d %q", response.Code, response.Body.String())
	}
}

func TestBackupRoutesListCreateAndRestore(t *testing.T) {
	backend := &testBackend{
		backups: []Backup{{ID: "backup-1", ProjectUID: "project-a", Trigger: "manual"}},
		op:      Operation{ID: "operation", Status: "ACCEPTED"},
	}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/backups", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "backup-1") {
		t.Fatalf("backup list = %d %q", response.Code, response.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-a/backups", strings.NewReader(`{"operation_id":"create-1","relative_paths":["compose.yaml",".env"]}`))
	create.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusAccepted || backend.create.ProjectUID != "project-a" || backend.create.ID != "create-1" || len(backend.create.RelativePaths) != 2 {
		t.Fatalf("backup create = %d %q request=%+v", response.Code, response.Body.String(), backend.create)
	}

	restore := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-a/backups/backup-1/restore", strings.NewReader(`{"operation_id":"restore-1"}`))
	restore.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, restore)
	if response.Code != http.StatusAccepted || backend.restore.ProjectUID != "project-a" || backend.restore.BackupID != "backup-1" || backend.restore.ID != "restore-1" {
		t.Fatalf("backup restore = %d %q request=%+v", response.Code, response.Body.String(), backend.restore)
	}
}

func TestFileTransportLimitIsExplicitHTTP413(t *testing.T) {
	handler := newTestHandler(t, &testBackend{err: fmt.Errorf("%w: encoded file cannot fit the 1 MiB Agent transport frame", ErrTooLarge)})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/files?path=compose.yaml", nil))
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "1 MiB") {
		t.Fatalf("limit response = %d %q", response.Code, response.Body.String())
	}
}

func TestOperationRequiresStrictBoundedJSONAndReturnsAccepted(t *testing.T) {
	backend := &testBackend{op: Operation{ID: "op-1", Status: "requested", Revision: 1}}
	handler := newTestHandler(t, backend)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"operation_id":"op-1","agent_id":"a","kind":"compose.down"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || backend.request.Kind != "compose.down" {
		t.Fatalf("response=%d %q request=%#v", response.Code, response.Body.String(), backend.request)
	}

	bad := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"operation_id":"op-1","agent_id":"a","kind":"x","secret":"leak"}`))
	bad.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, bad)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "leak") {
		t.Fatalf("strict error=%d %q", response.Code, response.Body.String())
	}
}

func TestOperationLookupAndCancelRoutesAreStrictAndUnambiguous(t *testing.T) {
	backend := &testBackend{
		op: Operation{ID: "op-1", Status: "running", Revision: 2},
		cancellation: OperationCancellation{Outcome: "ACCEPTED", Operation: Operation{
			ID: "op-1", Status: "canceled", Revision: 3,
		}},
	}
	handler := newTestHandler(t, backend)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-a/operations/op-1", nil))
	if response.Code != http.StatusOK || backend.getAgent != "agent-a" || backend.getID != "op-1" || !strings.Contains(response.Body.String(), `"revision":2`) {
		t.Fatalf("lookup = %d %q agent=%q operation=%q", response.Code, response.Body.String(), backend.getAgent, backend.getID)
	}

	cancel := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/operations/op-1/cancel", strings.NewReader(`{}`))
	cancel.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, cancel)
	if response.Code != http.StatusOK || backend.cancelAgent != "agent-a" || backend.cancelID != "op-1" || !strings.Contains(response.Body.String(), `"outcome":"ACCEPTED"`) {
		t.Fatalf("cancel = %d %q agent=%q operation=%q", response.Code, response.Body.String(), backend.cancelAgent, backend.cancelID)
	}

	for _, test := range []struct {
		method, path, body, contentType string
		wantStatus                      int
	}{
		{http.MethodGet, "/api/v1/agents/agent-a/operations/op-1?extra=true", "", "", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/agents/agent-a/operations/op-1/cancel?extra=true", `{}`, "application/json", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/agents/agent-a/operations/op-1/cancel", `{"reason":"TIMEOUT"}`, "application/json", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/agents/agent-a/operations/op-1/cancel", "", "application/json", http.StatusBadRequest},
		{http.MethodDelete, "/api/v1/agents/agent-a/operations/op-1", "", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/agents/agent-a/operations/op-1/cancel", "", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/agents/agent-a/operations/op-1/extra", "", "", http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		if test.contentType != "" {
			request.Header.Set("Content-Type", test.contentType)
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus {
			t.Errorf("%s %s = %d %q, want %d", test.method, test.path, response.Code, response.Body.String(), test.wantStatus)
		}
	}
}

func TestStartingOperationNeverImplicitlyCancelsOnBrowserDisconnect(t *testing.T) {
	backend := &testBackend{op: Operation{ID: "op-1", Status: "requested", Revision: 1}}
	handler := newTestHandler(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"operation_id":"op-1","agent_id":"agent-a","kind":"compose.up"}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if backend.cancelID != "" || backend.cancelAgent != "" {
		t.Fatalf("browser disconnect synthesized cancellation: agent=%q operation=%q", backend.cancelAgent, backend.cancelID)
	}
}

func TestErrorsDoNotDiscloseBackendDetails(t *testing.T) {
	handler := newTestHandler(t, &testBackend{err: errors.New("database contains super-secret")})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "super-secret") {
		t.Fatalf("response=%d %q", response.Code, response.Body.String())
	}
	var got problem
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got.Code != "INTERNAL" {
		t.Fatalf("problem=%#v err=%v", got, err)
	}
}

func TestLiveSSEIsNoStoreNonResumableAndBounded(t *testing.T) {
	logs := &sliceLogStream{events: []LogEvent{{
		Data: []byte("secret-bearing-log\n"), Stream: "STDOUT", LineCount: 1,
		DroppedBytes: 12, DroppedLines: 2, Terminal: true,
	}}}
	handler := newTestHandler(t, &testBackend{logStream: logs})
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/live/logs?agent_id=agent&container_id="+strings.Repeat("a", 64)+"&tail=100", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		!strings.Contains(response.Header().Get("Cache-Control"), "no-store") || response.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("SSE headers/status = %d %+v", response.Code, response.Header())
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: log") || strings.Contains(body, "id:") || strings.Contains(body, "retry:") || strings.Contains(body, "secret-bearing-log") {
		t.Fatalf("SSE framing leaked raw/resume data: %q", body)
	}
	if !logs.closed {
		t.Fatal("completed SSE did not close backend stream")
	}

	resume := httptest.NewRequest(http.MethodGet,
		"/api/v1/live/logs?agent_id=agent&container_id="+strings.Repeat("a", 64), nil)
	resume.Header.Set("Last-Event-ID", "42")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, resume)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "cannot be resumed") {
		t.Fatalf("resume response = %d %q", response.Code, response.Body.String())
	}
}

func TestStatsSSEContainsCurrentSampleOnly(t *testing.T) {
	stats := &sliceStatsStream{samples: []StatsSample{{ContainerID: strings.Repeat("b", 64), CPUPercent: 25, MemoryUsage: 10}}}
	backend := &testBackend{statsStream: stats}
	handler := newTestHandler(t, backend)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/live/stats?agent_id=agent&container_id="+strings.Repeat("b", 64), nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: stats") ||
		!strings.Contains(response.Body.String(), `"cpu_percent":25`) || !stats.closed {
		t.Fatalf("stats SSE = %d %q closed=%v", response.Code, response.Body.String(), stats.closed)
	}
}

type cancelLogStream struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (s *cancelLogStream) Recv(ctx context.Context) (LogEvent, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return LogEvent{}, ctx.Err()
}
func (s *cancelLogStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func TestBrowserDisconnectCancelsLiveBackendImmediately(t *testing.T) {
	stream := &cancelLogStream{started: make(chan struct{}), closed: make(chan struct{})}
	handler := newTestHandler(t, &testBackend{logStream: stream})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/live/logs?agent_id=agent&container_id="+strings.Repeat("c", 64), nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-stream.started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not stop on browser disconnect")
	}
	select {
	case <-stream.closed:
	default:
		t.Fatal("browser disconnect did not close backend stream")
	}
}

type countingLogStream struct {
	mu       sync.Mutex
	receives int
}

func (s *countingLogStream) Recv(ctx context.Context) (LogEvent, error) {
	s.mu.Lock()
	s.receives++
	call := s.receives
	s.mu.Unlock()
	if call == 1 {
		return LogEvent{Data: []byte("one\n")}, nil
	}
	<-ctx.Done()
	return LogEvent{}, ctx.Err()
}
func (*countingLogStream) Close() error { return nil }
func (s *countingLogStream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receives
}

type slowResponseWriter struct {
	header  http.Header
	mu      sync.Mutex
	writes  int
	blocked chan struct{}
	release chan struct{}
}

func newSlowResponseWriter() *slowResponseWriter {
	return &slowResponseWriter{header: make(http.Header), blocked: make(chan struct{}), release: make(chan struct{})}
}
func (w *slowResponseWriter) Header() http.Header { return w.header }
func (*slowResponseWriter) WriteHeader(int)       {}
func (*slowResponseWriter) Flush()                {}
func (w *slowResponseWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	write := w.writes
	w.mu.Unlock()
	if write == 2 {
		close(w.blocked)
		<-w.release
	}
	return len(payload), nil
}

func TestSlowBrowserDoesNotCreateServerSidePrefetchQueue(t *testing.T) {
	stream := &countingLogStream{}
	handler := newTestHandler(t, &testBackend{logStream: stream})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/live/logs?agent_id=agent&container_id="+strings.Repeat("d", 64), nil).WithContext(ctx)
	writer := newSlowResponseWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, request)
		close(done)
	}()
	select {
	case <-writer.blocked:
	case <-time.After(time.Second):
		t.Fatal("test writer did not block live event")
	}
	if got := stream.count(); got != 1 {
		t.Fatalf("slow browser prefetched %d transport events, want one in flight", got)
	}
	cancel()
	close(writer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow browser handler did not terminate after disconnect")
	}
}

func TestEmbeddedLiveUIIsAccessibleAndUsesFetchWithoutReconnect(t *testing.T) {
	handler := newTestHandler(t, &testBackend{})
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	html := index.Body.String()
	for _, required := range []string{
		`<label for="logs-agent">`, `role="log"`, `aria-live="polite"`,
		`<label for="metrics-container">`, `role="img"`, `<title id="stats-chart-title">`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("embedded UI missing accessible markup %q", required)
		}
	}
	script := httptest.NewRecorder()
	handler.ServeHTTP(script, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	js := script.Body.String()
	if !strings.Contains(js, "MAX_STATS_SAMPLES = 120") || !strings.Contains(js, "fetch(url") || !strings.Contains(js, "const warning = value.enabled && reason") ||
		!strings.Contains(js, "button.title = capability.reason") || strings.Contains(js, "EventSource") ||
		strings.Contains(js, "setInterval") || strings.Contains(js, "setTimeout") {
		t.Fatalf("browser streaming contract missing or reconnect present")
	}
}
