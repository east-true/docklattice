package registrationhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/east-true/dockpilot/internal/identity"
	"github.com/east-true/dockpilot/internal/registration"
)

const maxBodyBytes = 128 << 10

type Handler struct {
	registration *registration.Service
	identities   *identity.Manager
	archive      ArchiveIdentity
	now          func() time.Time
}

func NewHandler(service *registration.Service, identities *identity.Manager, archive ArchiveIdentity) (*Handler, error) {
	if service == nil || identities == nil || archive.ServerIdentityID == "" || archive.Generation == 0 || archive.AuditArchiveID == "" {
		return nil, errors.New("registrationhttp: registration, identity, and archive are required")
	}
	if archive.ServerIdentityID != identities.ServerIdentityID() {
		return nil, errors.New("registrationhttp: archive belongs to another Server identity")
	}
	return &Handler{registration: service, identities: identities, archive: archive, now: time.Now}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.TLS == nil {
		writeError(w, http.StatusUpgradeRequired, "TLS_REQUIRED")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	switch r.URL.Path {
	case RegisterPath:
		h.register(w, r)
	case RenewPath:
		h.renew(w, r)
	case ActivatePath:
		h.activate(w, r)
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND")
	}
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var request RegisterRequest
	if err := decodeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	registrationRequest := registration.Request{
		JoinToken: request.JoinToken, AgentID: request.AgentID,
		DisplayName: request.DisplayName, Metadata: request.Metadata,
	}
	if request.ExpiredCredential != nil {
		registrationRequest.Reuse = &registration.ReuseRequest{ExpiredCredential: *request.ExpiredCredential}
	}
	result, err := h.registration.Register(r.Context(), registrationRequest)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "REGISTRATION_REJECTED")
		return
	}
	writeJSON(w, http.StatusCreated, CredentialResponse{Credential: result.Credential, Archive: h.archive})
}

func (h *Handler) renew(w http.ResponseWriter, r *http.Request) {
	var request RenewRequest
	if err := decodeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	now := h.now().UTC()
	if !request.Current.RenewalDue(now) {
		writeError(w, http.StatusConflict, "RENEWAL_NOT_DUE")
		return
	}
	replacement, err := h.identities.RenewCredential(request.Current, now)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "CREDENTIAL_REJECTED")
		return
	}
	writeJSON(w, http.StatusOK, CredentialResponse{Credential: replacement, Archive: h.archive})
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	var request ActivateRequest
	if err := decodeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	now := h.now().UTC()
	if request.Previous.AgentID == "" || request.Previous.AgentID != request.Active.AgentID ||
		request.Previous.CredentialID == request.Active.CredentialID {
		writeError(w, http.StatusUnauthorized, "ACTIVATION_REJECTED")
		return
	}
	if err := h.identities.VerifyCredential(request.Active, now); err != nil {
		writeError(w, http.StatusUnauthorized, "ACTIVATION_REJECTED")
		return
	}
	if err := h.identities.RevokeCredential(request.Previous, now, "credential renewed and replacement activated"); err != nil &&
		!errors.Is(err, identity.ErrExpiredCredential) {
		writeError(w, http.StatusUnauthorized, "ACTIVATION_REJECTED")
		return
	}
	writeJSON(w, http.StatusOK, ActivateResponse{Activated: true})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("application/json is required")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing request data")
	}
	return nil
}

type errorResponse struct {
	Code string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func strictJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing response data")
	}
	return nil
}
