package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRequestBytes          = 64 << 10
	maxProjectFileBytes      = 1 << 20
	maxFileWriteRequestBytes = 6*maxProjectFileBytes + maxRequestBytes
	maxSSEEventBytes         = 2 << 20
	maxTailLines             = 10_000
)

type Handler struct {
	backend     Backend
	static      http.Handler
	diagnostics io.Writer
}

func New(backend Backend) (*Handler, error) {
	return NewWithDiagnostics(backend, nil)
}

// NewWithDiagnostics attaches a writer that receives one bounded line for every
// request answered with 500. The client is deliberately told nothing beyond
// "request failed", so without this the only unexpected server-side failure mode
// leaves no trace anywhere.
func NewWithDiagnostics(backend Backend, diagnostics io.Writer) (*Handler, error) {
	if backend == nil {
		return nil, errors.New("webui: backend is required")
	}
	assets, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return nil, fmt.Errorf("webui: open embedded assets: %w", err)
	}
	return &Handler{backend: backend, static: http.FileServer(http.FS(assets)), diagnostics: diagnostics}, nil
}

// internalErrorLogLimit bounds one diagnostic line so an error carrying
// Agent-provided text cannot turn the Server's output into unbounded logging.
const internalErrorLogLimit = 512

// statusClientClosedRequest is nginx's 499. Go has no constant for it and no
// RFC defines one, but a real code is still better than reusing 500 for a
// client that hung up: the difference matters in an access log even though the
// response itself has nowhere to go.
const statusClientClosedRequest = 499

func (h *Handler) logInternalError(err error) {
	if h.diagnostics == nil || err == nil {
		return
	}
	message := err.Error()
	if len(message) > internalErrorLogLimit {
		message = message[:internalErrorLogLimit]
	}
	message = strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' {
			return ' '
		}
		return char
	}, message)
	fmt.Fprintf(h.diagnostics, "api request failed: %s\n", message)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.serveAPI(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET and HEAD are allowed for UI assets")
		return
	}
	// The UI is a client-side router. Unknown extensionless routes receive the
	// shell; missing concrete assets stay 404 rather than being served as HTML.
	if r.URL.Path != "/" && !strings.Contains(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".") {
		clone := r.Clone(r.Context())
		clone.URL.Path = "/"
		h.static.ServeHTTP(w, clone)
		return
	}
	h.static.ServeHTTP(w, r)
}

func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request) {
	// API responses may contain current operation output or explicitly revealed
	// environment values. They are live views and must not be retained by an
	// intermediary or browser cache.
	w.Header().Set("Cache-Control", "no-store")
	projectUID, resource, tail, projectRoute := splitProjectRoute(r.URL.Path)
	agentID, operationID, cancelRoute, operationRoute := splitOperationRoute(r.URL.Path)
	hostAgentID, inventoryResource, hostInventoryRoute := splitHostInventoryRoute(r.URL.Path)
	hostObjectAgentID, hostObjectResource, hostObjectID, hostObjectRoute := splitHostObjectRoute(r.URL.Path)
	auditAgentID, hostAuditRoute := splitHostAuditRoute(r.URL.Path)
	switch {
	case operationRoute && !cancelRoute && r.Method == http.MethodGet:
		if !requireNoQuery(w, r) {
			return
		}
		value, err := h.backend.GetOperation(r.Context(), agentID, operationID)
		h.respond(w, value, err)
	case operationRoute && cancelRoute && r.Method == http.MethodPost:
		if !requireNoQuery(w, r) {
			return
		}
		var request struct{}
		if err := decodeJSON(w, r, &request); err != nil {
			writeDecodeProblem(w, err)
			return
		}
		value, err := h.backend.CancelOperation(r.Context(), agentID, operationID)
		h.respond(w, value, err)
	case operationRoute:
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "operation route requires GET, or POST for cancel")
	case hostAuditRoute && r.Method == http.MethodGet:
		request, err := decodeAuditPageRequest(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		value, err := h.backend.HostAudit(r.Context(), auditAgentID, request)
		h.respond(w, value, err)
	case hostAuditRoute:
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "host audit route requires GET")
	case hostObjectRoute && r.Method == http.MethodGet:
		if !requireEmptyGET(w, r) {
			return
		}
		var value any
		var err error
		switch hostObjectResource {
		case "containers":
			value, err = h.backend.HostContainer(r.Context(), hostObjectAgentID, hostObjectID)
		case "images":
			value, err = h.backend.HostImage(r.Context(), hostObjectAgentID, hostObjectID)
		case "networks":
			value, err = h.backend.HostNetwork(r.Context(), hostObjectAgentID, hostObjectID)
		case "volumes":
			value, err = h.backend.HostVolume(r.Context(), hostObjectAgentID, hostObjectID)
		}
		h.respond(w, value, err)
	case hostObjectRoute:
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "host object Inspector routes require GET")
	case hostInventoryRoute && r.Method == http.MethodGet:
		if !requireEmptyGET(w, r) {
			return
		}
		var value any
		var err error
		switch inventoryResource {
		case "containers":
			value, err = h.backend.HostContainers(r.Context(), hostAgentID)
		case "images":
			value, err = h.backend.HostImages(r.Context(), hostAgentID)
		case "networks":
			value, err = h.backend.HostNetworks(r.Context(), hostAgentID)
		case "volumes":
			value, err = h.backend.HostVolumes(r.Context(), hostAgentID)
		}
		h.respond(w, value, err)
	case hostInventoryRoute:
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "host inventory routes require GET")
	case r.URL.Path == "/api/v1/live/logs" && r.Method == http.MethodGet:
		h.serveLogs(w, r)
	case r.URL.Path == "/api/v1/live/stats" && r.Method == http.MethodGet:
		h.serveStats(w, r)
	case r.URL.Path == "/api/v1/live/matrix" && r.Method == http.MethodGet:
		h.serveMatrix(w, r)
	case r.URL.Path == "/api/v1/dashboard" && r.Method == http.MethodGet:
		if !requireEmptyGET(w, r) {
			return
		}
		value, err := h.backend.Dashboard(r.Context())
		h.respond(w, value, err)
	case strings.HasPrefix(r.URL.Path, "/api/v1/hosts/") && r.Method == http.MethodGet:
		if !requireEmptyGET(w, r) {
			return
		}
		id, ok := singlePathValue(r.URL.Path, "/api/v1/hosts/")
		if !ok {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "a single host id is required")
			return
		}
		value, err := h.backend.Host(r.Context(), id)
		h.respond(w, value, err)
	case projectRoute && resource == "environment" && len(tail) == 0 && r.Method == http.MethodGet:
		reveal, ok := decodeRevealOnlyQuery(w, r)
		if !ok {
			return
		}
		entries, err := h.backend.ProjectEnvironment(r.Context(), projectUID)
		if err != nil {
			h.respond(w, nil, err)
			return
		}
		for i := range entries {
			if entries[i].Secret && !reveal {
				entries[i].Value = "********"
			}
		}
		writeJSON(w, http.StatusOK, entries)
	case projectRoute && resource == "runtime" && len(tail) == 0 && r.Method == http.MethodGet:
		if !requireEmptyGET(w, r) {
			return
		}
		value, err := h.backend.ProjectRuntime(r.Context(), projectUID)
		h.respond(w, value, err)
	case projectRoute && resource == "compose" && len(tail) == 1 && tail[0] == "ps" && r.Method == http.MethodGet:
		request, err := decodeComposeQuery(r, true)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		value, err := h.backend.ProjectComposePS(r.Context(), projectUID, request)
		h.respond(w, value, err)
	case projectRoute && resource == "compose" && len(tail) == 1 && tail[0] == "config" && r.Method == http.MethodGet:
		request, err := decodeComposeQuery(r, false)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		value, err := h.backend.ProjectComposeConfig(r.Context(), projectUID, request)
		h.respond(w, value, err)
	case projectRoute && resource == "compose" && len(tail) == 1 && tail[0] == "logs" && r.Method == http.MethodGet:
		h.serveProjectComposeLogs(w, r, projectUID)
	case projectRoute && resource == "compose" && len(tail) == 1 && (tail[0] == "ps" || tail[0] == "config" || tail[0] == "logs"):
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Compose query routes require GET")
	case projectRoute && resource == "activity" && len(tail) == 0 && r.Method == http.MethodGet:
		request, err := decodeAuditPageRequest(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		value, err := h.backend.ProjectActivity(r.Context(), projectUID, request)
		h.respond(w, value, err)
	case projectRoute && resource == "activity" && len(tail) == 0:
		writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "project activity route requires GET")
	case projectRoute && resource == "files" && len(tail) == 0 && r.Method == http.MethodGet:
		h.serveProjectFile(w, r, projectUID)
	case projectRoute && resource == "files" && len(tail) == 0 && r.Method == http.MethodPut:
		h.serveProjectFileWrite(w, r, projectUID)
	case projectRoute && resource == "backups" && len(tail) == 0 && r.Method == http.MethodGet:
		if !requireNoQuery(w, r) {
			return
		}
		value, err := h.backend.ProjectBackups(r.Context(), projectUID)
		h.respond(w, value, err)
	case projectRoute && resource == "backups" && len(tail) == 0 && r.Method == http.MethodPost:
		h.serveBackupCreate(w, r, projectUID)
	case projectRoute && resource == "backups" && len(tail) == 2 && tail[1] == "restore" && r.Method == http.MethodPost:
		h.serveBackupRestore(w, r, projectUID, tail[0])
	case r.URL.Path == "/api/v1/operations" && r.Method == http.MethodGet:
		request, err := decodeOperationListRequest(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		value, err := h.backend.ListOperations(r.Context(), request)
		h.respond(w, value, err)
	case r.URL.Path == "/api/v1/operations" && r.Method == http.MethodPost:
		var request OperationRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeDecodeProblem(w, err)
			return
		}
		if request.ID == "" || request.AgentID == "" || request.Kind == "" {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "operation_id, agent_id, and kind are required")
			return
		}
		value, err := h.backend.StartOperation(r.Context(), request)
		if err != nil {
			h.respond(w, nil, err)
			return
		}
		writeJSON(w, http.StatusAccepted, value)
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method is not allowed")
			return
		}
		writeProblem(w, http.StatusNotFound, "NOT_FOUND", "API route not found")
	}
}

func decodeOperationListRequest(r *http.Request) (OperationListRequest, error) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" || len(values) != 1 {
			return OperationListRequest{}, errors.New("only one limit query value is allowed")
		}
	}
	request := OperationListRequest{}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			return OperationListRequest{}, errors.New("limit must be an integer between 1 and 200")
		}
		request.Limit = limit
	}
	return request, nil
}

func (h *Handler) serveProjectFile(w http.ResponseWriter, r *http.Request, projectUID string) {
	query := r.URL.Query()
	for key, values := range query {
		if (key != "path" && key != "reveal") || len(values) != 1 {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "only one path and optional reveal query value are allowed")
			return
		}
	}
	relativePath := query.Get("path")
	if relativePath == "" {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "path is required")
		return
	}
	reveal := false
	if value, exists := query["reveal"]; exists {
		if value[0] != "true" && value[0] != "false" {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "reveal must be true or false")
			return
		}
		reveal = value[0] == "true"
	}
	file, err := h.backend.ProjectFile(r.Context(), projectUID, relativePath)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	if file.Secret && !reveal {
		file.Content = "********"
	}
	writeJSON(w, http.StatusOK, file)
}

func (h *Handler) serveProjectFileWrite(w http.ResponseWriter, r *http.Request, projectUID string) {
	if !requireNoQuery(w, r) {
		return
	}
	var request FileWriteRequest
	if err := decodeJSONLimit(w, r, &request, maxFileWriteRequestBytes); err != nil {
		writeDecodeProblem(w, err)
		return
	}
	defer func() { request.Content = "" }()
	request.ProjectUID = projectUID
	value, err := h.backend.WriteProjectFile(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, value)
}

func (h *Handler) serveBackupCreate(w http.ResponseWriter, r *http.Request, projectUID string) {
	if !requireNoQuery(w, r) {
		return
	}
	var request BackupCreateRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeProblem(w, err)
		return
	}
	request.ProjectUID = projectUID
	value, err := h.backend.CreateBackup(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, value)
}

func (h *Handler) serveBackupRestore(w http.ResponseWriter, r *http.Request, projectUID, backupID string) {
	if !requireNoQuery(w, r) {
		return
	}
	var request BackupRestoreRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeProblem(w, err)
		return
	}
	request.ProjectUID, request.BackupID = projectUID, backupID
	value, err := h.backend.RestoreBackup(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, value)
}

// decodeRevealOnlyQuery parses the single query parameter this route
// understands. An unrecognised key or a reveal value that is not exactly true
// or false is refused rather than ignored: a silently dropped parameter lets a
// caller believe it asked for revealed values and receive masked ones instead.
func decodeRevealOnlyQuery(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is not allowed")
		return false, false
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "reveal" || len(values) != 1 {
			writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "only one reveal query value is allowed")
			return false, false
		}
	}
	value, exists := query["reveal"]
	if !exists {
		return false, true
	}
	if value[0] != "true" && value[0] != "false" {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "reveal must be true or false")
		return false, false
	}
	return value[0] == "true", true
}

func requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "query parameters are not allowed")
		return false
	}
	return true
}

func requireEmptyGET(w http.ResponseWriter, r *http.Request) bool {
	if !requireNoQuery(w, r) {
		return false
	}
	// Do not accept an alternate query envelope in a GET body. Inbound fixed or
	// chunked bodies are rejected from framing metadata without blocking to read
	// attacker-controlled bytes.
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is not allowed")
		return false
	}
	return true
}

func (h *Handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	request, err := decodeLiveRequest(r, true)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	stream, err := h.backend.OpenLogs(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "streaming response is unavailable")
		return
	}
	prepareSSE(w)
	if _, err := io.WriteString(w, ": stream-open\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		event, err := stream.Recv(r.Context())
		if err != nil {
			return
		}
		terminal := event.Terminal
		if err := writeSSE(w, "log", event); err != nil {
			clear(event.Data)
			return
		}
		clear(event.Data)
		flusher.Flush()
		if terminal {
			return
		}
	}
}

// serveProjectComposeLogs is intentionally separate from the container-ID
// live-log route: the browser selects a catalogued project and optional
// catalogued service names, never an agent or arbitrary command target.
func (h *Handler) serveProjectComposeLogs(w http.ResponseWriter, r *http.Request, projectUID string) {
	request, err := decodeProjectLogRequest(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	stream, err := h.backend.OpenProjectLogs(r.Context(), projectUID, request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "streaming response is unavailable")
		return
	}
	prepareSSE(w)
	if _, err := io.WriteString(w, ": stream-open\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		event, err := stream.Recv(r.Context())
		if err != nil {
			return
		}
		terminal := event.Terminal
		if err := writeSSE(w, "log", event); err != nil {
			clear(event.Data)
			return
		}
		clear(event.Data)
		flusher.Flush()
		if terminal {
			return
		}
	}
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	request, err := decodeLiveRequest(r, false)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	stream, err := h.backend.OpenStats(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "streaming response is unavailable")
		return
	}
	prepareSSE(w)
	if _, err := io.WriteString(w, ": stream-open\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		sample, err := stream.Recv(r.Context())
		if err != nil {
			return
		}
		if err := writeSSE(w, "stats", sample); err != nil {
			return
		}
		flusher.Flush()
	}
}

// serveMatrix streams one host's whole picture. It holds no state and decides
// nothing: the Backend has already settled membership, context and aggregation,
// and normalizing any of it here - filling in a stale reason, hiding a pending
// container, merging the two dropped-frame counters - would quietly contradict
// what the Server measured.
func (h *Handler) serveMatrix(w http.ResponseWriter, r *http.Request) {
	agentID, err := decodeMatrixRequest(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	stream, err := h.backend.OpenMatrix(r.Context(), agentID)
	if err != nil {
		// A host that cannot be watched answers here, before any SSE body is
		// written, so the browser gets a status and a reason rather than an
		// open stream that never produces a frame.
		h.respond(w, nil, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "streaming response is unavailable")
		return
	}
	prepareSSE(w)
	if _, err := io.WriteString(w, ": stream-open\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		frame, err := stream.Recv(r.Context())
		if err != nil {
			return
		}
		if err := writeSSE(w, "matrix", frame); err != nil {
			return
		}
		flusher.Flush()
	}
}

// decodeMatrixRequest accepts one host and nothing else. The frame is the whole
// host by design, so there is no container or service selector to offer:
// narrowing is the viewer's job, and a second endpoint carrying the same data
// in a different shape would be a second thing to keep correct.
func decodeMatrixRequest(r *http.Request) (string, error) {
	if r.Header.Get("Last-Event-ID") != "" {
		return "", errors.New("live streams cannot be resumed")
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "agent_id" || len(values) != 1 {
			return "", fmt.Errorf("unsupported or repeated live-stream parameter %q", key)
		}
	}
	agentID := query.Get("agent_id")
	if agentID == "" {
		return "", errors.New("agent_id is required")
	}
	return agentID, nil
}

func decodeLiveRequest(r *http.Request, logs bool) (LiveRequest, error) {
	if r.Header.Get("Last-Event-ID") != "" {
		return LiveRequest{}, errors.New("live streams cannot be resumed")
	}
	allowed := map[string]bool{"agent_id": true, "container_id": true}
	if logs {
		for _, key := range []string{"follow", "tail", "stdout", "stderr", "timestamps"} {
			allowed[key] = true
		}
	}
	query := r.URL.Query()
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return LiveRequest{}, fmt.Errorf("unsupported or repeated live-stream parameter %q", key)
		}
	}
	request := LiveRequest{AgentID: query.Get("agent_id"), ContainerID: query.Get("container_id")}
	if request.AgentID == "" || request.ContainerID == "" {
		return LiveRequest{}, errors.New("agent_id and container_id are required")
	}
	if !logs {
		return request, nil
	}
	request.Follow = true
	request.ShowStdout, request.ShowStderr = true, true
	var err error
	if value := query.Get("follow"); value != "" {
		request.Follow, err = strconv.ParseBool(value)
		if err != nil {
			return LiveRequest{}, errors.New("follow must be true or false")
		}
	}
	for key, target := range map[string]*bool{
		"stdout": &request.ShowStdout, "stderr": &request.ShowStderr, "timestamps": &request.Timestamps,
	} {
		if value := query.Get(key); value != "" {
			*target, err = strconv.ParseBool(value)
			if err != nil {
				return LiveRequest{}, fmt.Errorf("%s must be true or false", key)
			}
		}
	}
	if !request.ShowStdout && !request.ShowStderr {
		return LiveRequest{}, errors.New("at least one of stdout or stderr must be enabled")
	}
	if value := query.Get("tail"); value != "" {
		request.TailLines, err = strconv.ParseUint(value, 10, 64)
		if err != nil || request.TailLines > maxTailLines {
			return LiveRequest{}, fmt.Errorf("tail must be between 0 and %d", maxTailLines)
		}
	}
	return request, nil
}

func decodeComposeQuery(r *http.Request, allowAll bool) (ComposeQuery, error) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return ComposeQuery{}, errors.New("request body is not allowed")
	}
	query := r.URL.Query()
	for key, values := range query {
		if (key != "service" && key != "all" && key != "reveal") || len(values) == 0 || key != "service" && len(values) != 1 {
			return ComposeQuery{}, fmt.Errorf("unsupported or repeated Compose query parameter %q", key)
		}
	}
	request := ComposeQuery{Services: append([]string{}, query["service"]...)}
	if raw, exists := query["all"]; exists {
		if !allowAll {
			return ComposeQuery{}, errors.New("all is valid only for compose ps")
		}
		if raw[0] != "true" && raw[0] != "false" {
			return ComposeQuery{}, errors.New("all must be true or false")
		}
		request.All = raw[0] == "true"
	}
	if raw, exists := query["reveal"]; exists {
		if allowAll {
			return ComposeQuery{}, errors.New("reveal is valid only for compose config")
		}
		if raw[0] != "true" && raw[0] != "false" {
			return ComposeQuery{}, errors.New("reveal must be true or false")
		}
		request.Reveal = raw[0] == "true"
	}
	if len(request.Services) > 256 {
		return ComposeQuery{}, errors.New("too many Compose services")
	}
	seen := make(map[string]struct{}, len(request.Services))
	for _, service := range request.Services {
		if !validComposeService(service) {
			return ComposeQuery{}, errors.New("invalid Compose service")
		}
		if _, duplicate := seen[service]; duplicate {
			return ComposeQuery{}, errors.New("duplicate Compose service")
		}
		seen[service] = struct{}{}
	}
	return request, nil
}

func decodeProjectLogRequest(r *http.Request) (ProjectLogRequest, error) {
	if r.Header.Get("Last-Event-ID") != "" {
		return ProjectLogRequest{}, errors.New("live streams cannot be resumed")
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return ProjectLogRequest{}, errors.New("request body is not allowed")
	}
	query := r.URL.Query()
	for key, values := range query {
		if (key != "service" && key != "container_id" && key != "follow" && key != "tail" && key != "timestamps" && key != "since" && key != "until") || len(values) == 0 || key != "service" && len(values) != 1 {
			return ProjectLogRequest{}, fmt.Errorf("unsupported or repeated Compose log parameter %q", key)
		}
	}
	request := ProjectLogRequest{Services: append([]string(nil), query["service"]...), Follow: true}
	request.ContainerID = query.Get("container_id")
	if request.ContainerID != "" && (len(request.ContainerID) != 64 || len(request.Services) != 0) {
		return ProjectLogRequest{}, errors.New("select either one canonical Container ID or services")
	}
	if len(request.Services) > 256 {
		return ProjectLogRequest{}, errors.New("too many Compose services")
	}
	seen := make(map[string]struct{}, len(request.Services))
	for _, service := range request.Services {
		if !validComposeService(service) {
			return ProjectLogRequest{}, errors.New("invalid Compose service")
		}
		if _, duplicate := seen[service]; duplicate {
			return ProjectLogRequest{}, errors.New("duplicate Compose service")
		}
		seen[service] = struct{}{}
	}
	for key, target := range map[string]*bool{"follow": &request.Follow, "timestamps": &request.Timestamps} {
		if raw, exists := query[key]; exists {
			if raw[0] != "true" && raw[0] != "false" {
				return ProjectLogRequest{}, fmt.Errorf("%s must be true or false", key)
			}
			*target = raw[0] == "true"
		}
	}
	if raw, exists := query["tail"]; exists {
		if raw[0] == "" || (len(raw[0]) > 1 && raw[0][0] == '0') {
			return ProjectLogRequest{}, fmt.Errorf("tail must be between 0 and %d", maxTailLines)
		}
		for _, character := range raw[0] {
			if character < '0' || character > '9' {
				return ProjectLogRequest{}, fmt.Errorf("tail must be between 0 and %d", maxTailLines)
			}
		}
		value, err := strconv.ParseUint(raw[0], 10, 64)
		if err != nil || value > maxTailLines {
			return ProjectLogRequest{}, fmt.Errorf("tail must be between 0 and %d", maxTailLines)
		}
		request.TailLines = value
	}
	for key, target := range map[string]*time.Time{"since": &request.Since, "until": &request.Until} {
		if raw, exists := query[key]; exists {
			value, err := time.Parse(time.RFC3339Nano, raw[0])
			if err != nil || len(raw[0]) > 64 {
				return ProjectLogRequest{}, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			*target = value.UTC()
		}
	}
	if !request.Since.IsZero() && !request.Until.IsZero() && request.Since.After(request.Until) {
		return ProjectLogRequest{}, errors.New("since must not be after until")
	}
	return request, nil
}

func validComposeService(service string) bool {
	if len(service) == 0 || len(service) > 128 || !utf8.ValidString(service) || strings.ContainsRune(service, 0) {
		return false
	}
	for index, character := range service {
		isLetter := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && (index == 0 || character != '.' && character != '_' && character != '-') {
			return false
		}
	}
	return true
}

func prepareSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeSSE(w io.Writer, event string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > maxSSEEventBytes {
		return errors.New("SSE event exceeds bounded message limit")
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	return err
}

func (h *Handler) respond(w http.ResponseWriter, value any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, value)
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, ErrConflict):
		writeProblem(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, ErrUnavailable):
		writeProblem(w, http.StatusServiceUnavailable, "CAPABILITY_UNAVAILABLE", err.Error())
	// Contention for the Server database is load, not a broken invariant. It
	// is a transient answer the caller may retry, so it must not be reported
	// as an internal failure.
	case errors.Is(err, ErrBusy):
		writeProblem(w, http.StatusServiceUnavailable, "SERVER_BUSY", err.Error())
	case errors.Is(err, ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, ErrTooLarge):
		writeProblem(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", err.Error())
	// A caller that hangs up cancels its own request context: a closed browser
	// tab, a log stream the reader walked away from, a curl that hit its
	// --max-time. Nothing reaches a connection that is already gone, so the
	// status written here is never read; what does survive is the diagnostic
	// line, and calling a client's own disconnect an internal Server failure
	// buries real failures under stream-teardown noise.
	case errors.Is(err, context.Canceled):
		writeProblem(w, statusClientClosedRequest, "CLIENT_CLOSED_REQUEST", "the client closed the request")
	default:
		// ErrCorruptData and anything else unmapped is a genuine Server-side
		// invariant failure, so 500 is the correct answer. It is recorded here
		// because the response deliberately carries no detail.
		h.logInternalError(err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL", "request failed")
	}
}

func singlePathValue(path, prefix string) (string, bool) {
	value := strings.TrimPrefix(path, prefix)
	return value, value != "" && !strings.Contains(value, "/")
}

func splitHostInventoryRoute(path string) (agentID, resource string, ok bool) {
	if !strings.HasPrefix(path, "/api/v1/hosts/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/hosts/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	switch parts[1] {
	case "containers", "images", "networks", "volumes":
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
}

func splitHostObjectRoute(path string) (agentID, resource, objectID string, ok bool) {
	if !strings.HasPrefix(path, "/api/v1/hosts/") {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/hosts/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", "", "", false
	}
	switch parts[1] {
	case "containers", "images", "networks", "volumes":
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}

func splitHostAuditRoute(path string) (agentID string, ok bool) {
	if !strings.HasPrefix(path, "/api/v1/hosts/") {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/hosts/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "audit" {
		return parts[0], true
	}
	return "", false
}

func decodeAuditPageRequest(r *http.Request) (AuditPageRequest, error) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return AuditPageRequest{}, errors.New("request body is not allowed")
	}
	query := r.URL.Query()
	for key, values := range query {
		if (key != "limit" && key != "cursor" && key != "from" && key != "until" && key != "resource" && key != "kind" && key != "actor") || len(values) != 1 {
			return AuditPageRequest{}, errors.New("unsupported or repeated Audit query value")
		}
	}
	request := AuditPageRequest{Limit: DefaultAuditPageSize}
	if raw, exists := query["limit"]; exists {
		value, err := parseCanonicalUint(raw[0])
		if err != nil || value > MaxAuditPageSize {
			return AuditPageRequest{}, fmt.Errorf("limit must be between 1 and %d", MaxAuditPageSize)
		}
		request.Limit = int(value)
	}
	if raw, exists := query["cursor"]; exists {
		parts := strings.Split(raw[0], ":")
		if len(parts) != 2 {
			return AuditPageRequest{}, errors.New("cursor must be incarnation:seq")
		}
		incarnation, incarnationErr := parseCanonicalUint(parts[0])
		seq, seqErr := parseCanonicalUint(parts[1])
		if incarnationErr != nil || seqErr != nil || incarnation > math.MaxInt64 || seq > math.MaxInt64 {
			return AuditPageRequest{}, errors.New("cursor must contain bounded positive integers")
		}
		request.Cursor = &AuditCursor{Incarnation: incarnation, Seq: seq}
	}
	for key, target := range map[string]**time.Time{"from": &request.From, "until": &request.Until} {
		if raw, exists := query[key]; exists {
			value, err := time.Parse(time.RFC3339Nano, raw[0])
			if err != nil || len(raw[0]) > 64 {
				return AuditPageRequest{}, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			value = value.UTC()
			*target = &value
		}
	}
	request.Resource, request.Kind, request.Actor = query.Get("resource"), query.Get("kind"), query.Get("actor")
	for _, key := range []string{"resource", "kind", "actor"} {
		if raw, exists := query[key]; exists && (raw[0] == "" || len(raw[0]) > map[string]int{"resource": 1024, "kind": 128, "actor": 1024}[key] || !utf8.ValidString(raw[0]) || strings.ContainsRune(raw[0], 0)) {
			return AuditPageRequest{}, fmt.Errorf("%s filter is invalid", key)
		}
	}
	if request.From != nil && request.Until != nil && request.From.After(*request.Until) {
		return AuditPageRequest{}, errors.New("from must not be after until")
	}
	return request, nil
}

func parseCanonicalUint(value string) (uint64, error) {
	if value == "" || value[0] == '0' {
		return 0, errors.New("positive canonical integer required")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("positive canonical integer required")
		}
	}
	return strconv.ParseUint(value, 10, 64)
}

func splitProjectRoute(path string) (projectUID, resource string, tail []string, ok bool) {
	if !strings.HasPrefix(path, "/api/v1/projects/") {
		return "", "", nil, false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/projects/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", nil, false
	}
	for _, part := range parts {
		if part == "" {
			return "", "", nil, false
		}
	}
	return parts[0], parts[1], parts[2:], true
}

func splitOperationRoute(path string) (agentID, operationID string, cancel, ok bool) {
	if !strings.HasPrefix(path, "/api/v1/agents/") {
		return "", "", false, false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/agents/"), "/")
	if len(parts) != 3 && len(parts) != 4 || parts[0] == "" || parts[1] != "operations" || parts[2] == "" {
		return "", "", false, false
	}
	if len(parts) == 4 {
		if parts[3] != "cancel" {
			return "", "", false, false
		}
		cancel = true
	}
	return parts[0], parts[2], cancel, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeJSONLimit(w, r, target, maxRequestBytes)
}

func decodeJSONLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	payload, err := io.ReadAll(reader)
	defer clear(payload)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return fmt.Errorf("%w: JSON request exceeds %d bytes", ErrTooLarge, limit)
		}
		return fmt.Errorf("read JSON: %w", err)
	}
	if !utf8.Valid(payload) {
		return errors.New("decode JSON: request is not valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("decode JSON: trailing data")
	}
	return nil
}

func rejectDuplicateJSONFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("decode JSON: top-level value must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode JSON: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("decode JSON: object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("decode JSON: duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode JSON: %w", err)
		}
		clear(value)
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode JSON: trailing data")
	}
	return nil
}

func writeDecodeProblem(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTooLarge) {
		writeProblem(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", err.Error())
		return
	}
	writeProblem(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
}

type problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, problem{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
