package registrationhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("registrationhttp: an HTTPS Server base URL is required")
	}
	if client == nil {
		return nil, errors.New("registrationhttp: HTTP client with Server trust configuration is required")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: client}, nil
}

func (c *Client) Register(ctx context.Context, request RegisterRequest) (CredentialResponse, error) {
	var response CredentialResponse
	if err := c.post(ctx, RegisterPath, request, &response); err != nil {
		return CredentialResponse{}, err
	}
	if response.Credential.AgentID != request.AgentID || !validArchive(response.Archive) ||
		response.Credential.ServerIdentityID != response.Archive.ServerIdentityID {
		return CredentialResponse{}, fmt.Errorf("%w: inconsistent registration response identity", ErrProtocol)
	}
	return response, nil
}

func (c *Client) Renew(ctx context.Context, request RenewRequest) (CredentialResponse, error) {
	var response CredentialResponse
	if err := c.post(ctx, RenewPath, request, &response); err != nil {
		return CredentialResponse{}, err
	}
	if response.Credential.AgentID != request.Current.AgentID ||
		response.Credential.CredentialID == request.Current.CredentialID || !validArchive(response.Archive) ||
		response.Credential.ServerIdentityID != response.Archive.ServerIdentityID {
		return CredentialResponse{}, fmt.Errorf("%w: inconsistent renewal response identity", ErrProtocol)
	}
	return response, nil
}

func (c *Client) Activate(ctx context.Context, request ActivateRequest) error {
	var response ActivateResponse
	if err := c.post(ctx, ActivatePath, request, &response); err != nil {
		return err
	}
	if !response.Activated {
		return fmt.Errorf("%w: Server did not confirm activation", ErrProtocol)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, request, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("%w: encode request: %v", ErrProtocol, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: create request: %v", ErrProtocol, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("registrationhttp: request Server: %w", err)
	}
	defer httpResponse.Body.Close()
	limited := io.LimitReader(httpResponse.Body, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrProtocol, err)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("%w: response too large", ErrProtocol)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return fmt.Errorf("%w: HTTP %d", ErrRejected, httpResponse.StatusCode)
	}
	if err := strictJSON(body, response); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrProtocol, err)
	}
	return nil
}

func validArchive(archive ArchiveIdentity) bool {
	return archive.ServerIdentityID != "" && archive.Generation > 0 && archive.AuditArchiveID != ""
}
