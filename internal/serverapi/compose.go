package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/webui"
	"google.golang.org/grpc/status"
)

const (
	maxComposeServices    = 256
	maxComposeServiceSize = 128
	maxComposeLogTail     = 10_000
)

var composeQueryTimeout = 15 * time.Second

type agentComposeOutput struct {
	Output string `json:"output"`
}

func (b *Backend) ProjectComposePS(ctx context.Context, projectUID string, request webui.ComposeQuery) (webui.ComposeOutput, error) {
	if request.Reveal {
		return webui.ComposeOutput{}, fmt.Errorf("%w: reveal is valid only for compose config", webui.ErrInvalidRequest)
	}
	if err := validateComposeQuery(request); err != nil {
		return webui.ComposeOutput{}, err
	}
	return b.projectComposeQuery(ctx, projectUID, QueryComposePS, request)
}

func (b *Backend) ProjectComposeConfig(ctx context.Context, projectUID string, request webui.ComposeQuery) (webui.ComposeOutput, error) {
	if request.All {
		return webui.ComposeOutput{}, fmt.Errorf("%w: all is valid only for compose ps", webui.ErrInvalidRequest)
	}
	if err := validateComposeQuery(request); err != nil {
		return webui.ComposeOutput{}, err
	}
	return b.projectComposeQuery(ctx, projectUID, QueryComposeConfig, request)
}

func (b *Backend) OpenProjectLogs(ctx context.Context, projectUID string, request webui.ProjectLogRequest) (webui.LogStream, error) {
	if request.TailLines > maxComposeLogTail {
		return nil, fmt.Errorf("%w: Compose log tail exceeds %d lines", webui.ErrInvalidRequest, maxComposeLogTail)
	}
	if err := validateComposeQuery(webui.ComposeQuery{Services: request.Services}); err != nil {
		return nil, err
	}
	if request.ContainerID != "" {
		if !canonicalContainerID.MatchString(request.ContainerID) || len(request.Services) != 0 {
			return nil, fmt.Errorf("%w: project logs select either one Container or services", webui.ErrInvalidRequest)
		}
	}
	if !request.Since.IsZero() && !request.Until.IsZero() && request.Since.After(request.Until) {
		return nil, fmt.Errorf("%w: log since time must not be after until time", webui.ErrInvalidRequest)
	}
	access, err := b.projectAccess(ctx, projectUID, projectRead)
	if err != nil {
		return nil, err
	}
	if access.flags.Missing || access.flags.Stale || !access.flags.ComposeExecutable {
		reason := access.flags.CapabilityReason
		if reason == "" {
			reason = "Compose log capability is unavailable for this project"
		}
		return nil, fmt.Errorf("%w: %s", webui.ErrUnavailable, reason)
	}
	if request.ContainerID != "" {
		found := false
		for _, containerID := range access.flags.ContainerIDs {
			if containerID == request.ContainerID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: Container is not attached to this project", webui.ErrNotFound)
		}
	}
	session, err := b.activeSession(access.agentID)
	if err != nil {
		return nil, err
	}
	if session.Info().ProtocolVersion == producttransport.PreviousProductProtocolVersion &&
		(request.ContainerID != "" || !request.Since.IsZero() || !request.Until.IsZero()) {
		return nil, fmt.Errorf("%w: selected Container and Since/Until log controls require the current Dockpilot Agent", webui.ErrUnavailable)
	}
	stream, err := session.OpenLogs(ctx, producttransport.LogRequest{
		ProjectUID: projectUID, ContainerID: request.ContainerID, Services: append([]string(nil), request.Services...), Follow: request.Follow,
		TailLines: request.TailLines, ShowStdout: true, ShowStderr: true, Timestamps: request.Timestamps,
		Since: formatOptionalTime(request.Since), Until: formatOptionalTime(request.Until),
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &liveUnavailableError{agentID: access.agentID, action: "Compose logs", cause: err}
	}
	return &liveLogStream{stream: stream}, nil
}

func validateComposeQuery(request webui.ComposeQuery) error {
	if len(request.Services) > maxComposeServices {
		return fmt.Errorf("%w: too many Compose services", webui.ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(request.Services))
	for _, service := range request.Services {
		if len(service) > maxComposeServiceSize || !composeServiceName.MatchString(service) {
			return fmt.Errorf("%w: invalid Compose service", webui.ErrInvalidRequest)
		}
		if _, duplicate := seen[service]; duplicate {
			return fmt.Errorf("%w: duplicate Compose service", webui.ErrInvalidRequest)
		}
		seen[service] = struct{}{}
	}
	return nil
}

func (b *Backend) projectComposeQuery(ctx context.Context, projectUID, kind string, request webui.ComposeQuery) (webui.ComposeOutput, error) {
	access, err := b.projectAccess(ctx, projectUID, projectRead)
	if err != nil {
		return webui.ComposeOutput{}, err
	}
	if access.flags.Missing || access.flags.Stale || !access.flags.ComposeExecutable {
		reason := access.flags.CapabilityReason
		if reason == "" {
			reason = "Compose query capability is unavailable for this project"
		}
		return webui.ComposeOutput{}, fmt.Errorf("%w: %s", webui.ErrUnavailable, reason)
	}
	session, err := b.activeSession(access.agentID)
	if err != nil {
		return webui.ComposeOutput{}, err
	}
	services := append([]string{}, request.Services...)
	payload, err := json.Marshal(struct {
		Services []string `json:"services"`
		All      bool     `json:"all,omitempty"`
		Reveal   bool     `json:"reveal,omitempty"`
	}{Services: services, All: request.All, Reveal: request.Reveal})
	if err != nil {
		return webui.ComposeOutput{}, fmt.Errorf("serverapi: encode Compose query: %w", err)
	}
	defer clear(payload)
	queryCtx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	response, err := session.Query(queryCtx, producttransport.QueryRequest{Kind: kind, Target: projectUID, Payload: payload})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return webui.ComposeOutput{}, ctx.Err()
		}
		if status.Convert(err).Message() == "Agent query response exceeds 1 MiB" {
			return webui.ComposeOutput{}, fmt.Errorf("%w: Compose output cannot fit the Agent transport frame", webui.ErrTooLarge)
		}
		return webui.ComposeOutput{}, &liveUnavailableError{agentID: access.agentID, action: "Compose query", cause: err}
	}
	if err := queryCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return webui.ComposeOutput{}, ctx.Err()
		}
		return webui.ComposeOutput{}, &liveUnavailableError{agentID: access.agentID, action: "Compose query", cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return webui.ComposeOutput{}, &corruptDataError{boundary: "Agent Compose response", cause: errors.New("payload exceeds transport limit")}
	}
	var output agentComposeOutput
	if err := decodeStrictJSON(response.Payload, &output); err != nil {
		return webui.ComposeOutput{}, &corruptDataError{boundary: "Agent Compose response", cause: err}
	}
	if len(output.Output) > maxComposeOutputBytes || !utf8.ValidString(output.Output) {
		return webui.ComposeOutput{}, &corruptDataError{boundary: "Agent Compose response", cause: errors.New("invalid or oversized output")}
	}
	return webui.ComposeOutput{Output: output.Output}, nil
}
