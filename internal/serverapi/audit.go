package serverapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditstore"
	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/webui"
)

const maxAuditCoverageEntries = 500

func (b *Backend) HostAudit(ctx context.Context, agentID string, request webui.AuditPageRequest) (webui.AuditPage, error) {
	if !validOpaqueID(agentID) {
		return webui.AuditPage{}, fmt.Errorf("%w: valid Agent ID is required", webui.ErrInvalidRequest)
	}
	var storedID string
	if err := b.store.DB().QueryRowContext(ctx, `SELECT id FROM agents WHERE id = ?`, agentID).Scan(&storedID); errors.Is(err, sql.ErrNoRows) {
		return webui.AuditPage{}, fmt.Errorf("%w: Agent %q", webui.ErrNotFound, agentID)
	} else if err != nil {
		return webui.AuditPage{}, fmt.Errorf("serverapi: load audit Agent: %w", err)
	}
	if storedID != agentID {
		return webui.AuditPage{}, &corruptDataError{boundary: "audit Agent identity", cause: errors.New("stored Agent ID mismatch")}
	}
	return b.auditPage(ctx, agentID, "", request)
}

func (b *Backend) ProjectActivity(ctx context.Context, projectUID string, request webui.AuditPageRequest) (webui.AuditPage, error) {
	if !validOpaqueID(projectUID) {
		return webui.AuditPage{}, fmt.Errorf("%w: valid project UID is required", webui.ErrInvalidRequest)
	}
	var agentID string
	if err := b.store.DB().QueryRowContext(ctx, `SELECT agent_id FROM projects WHERE project_uid = ?`, projectUID).Scan(&agentID); errors.Is(err, sql.ErrNoRows) {
		return webui.AuditPage{}, fmt.Errorf("%w: project %q", webui.ErrNotFound, projectUID)
	} else if err != nil {
		return webui.AuditPage{}, fmt.Errorf("serverapi: load activity project: %w", err)
	}
	if !validOpaqueID(agentID) {
		return webui.AuditPage{}, &corruptDataError{boundary: "project activity identity", cause: errors.New("invalid stored Agent ID")}
	}
	return b.auditPage(ctx, agentID, projectUID, request)
}

func (b *Backend) auditPage(ctx context.Context, agentID, projectUID string, request webui.AuditPageRequest) (webui.AuditPage, error) {
	if b.audit == nil || b.auditArchiveID == "" {
		return webui.AuditPage{}, errors.New("serverapi: audit read model is not configured")
	}
	if request.Limit < 1 || request.Limit > webui.MaxAuditPageSize || !validAuditCursor(request.Cursor) || !validAuditFilters(request) {
		return webui.AuditPage{}, fmt.Errorf("%w: invalid audit page", webui.ErrInvalidRequest)
	}
	events, next, err := b.loadAuditEvents(ctx, agentID, projectUID, request)
	if err != nil {
		return webui.AuditPage{}, err
	}
	coverage, err := b.loadAuditCoverage(ctx, agentID)
	if err != nil {
		return webui.AuditPage{}, err
	}
	if len(events) != 0 && !coverage.Established {
		return webui.AuditPage{}, &corruptDataError{boundary: "audit coverage", cause: errors.New("canonical events exist without a coverage start")}
	}
	return webui.AuditPage{
		AgentID: agentID, ProjectUID: projectUID, Events: events, NextCursor: next, Coverage: coverage,
	}, nil
}

func validAuditFilters(request webui.AuditPageRequest) bool {
	if request.From != nil && request.Until != nil && request.From.After(*request.Until) {
		return false
	}
	for value, limit := range map[string]int{request.Resource: 1024, request.Kind: 128, request.Actor: 1024} {
		if len(value) > limit || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return true
}

func validAuditCursor(cursor *webui.AuditCursor) bool {
	return cursor == nil || cursor.Incarnation > 0 && cursor.Seq > 0 &&
		cursor.Incarnation <= math.MaxInt64 && cursor.Seq <= math.MaxInt64
}

func (b *Backend) loadAuditEvents(ctx context.Context, agentID, projectUID string, request webui.AuditPageRequest) ([]webui.AuditEvent, *webui.AuditCursor, error) {
	query := `
		SELECT e.agent_id, e.incarnation, e.seq, e.occurred_at, e.kind,
		       e.actor, e.project_uid, e.operation_id, e.resource_type, e.resource_id, e.action, e.metadata_json, p.agent_id
		FROM audit_events AS e
		LEFT JOIN projects AS p ON p.project_uid = e.project_uid
		WHERE e.agent_id = ?`
	args := []any{agentID}
	if projectUID != "" {
		query += ` AND e.project_uid = ?`
		args = append(args, projectUID)
	}
	if request.Cursor != nil {
		query += ` AND (e.incarnation > ? OR (e.incarnation = ? AND e.seq > ?))`
		args = append(args, request.Cursor.Incarnation, request.Cursor.Incarnation, request.Cursor.Seq)
	}
	if request.From != nil {
		query += ` AND e.occurred_at >= ?`
		args = append(args, request.From.UTC().Format(time.RFC3339Nano))
	}
	if request.Until != nil {
		query += ` AND e.occurred_at <= ?`
		args = append(args, request.Until.UTC().Format(time.RFC3339Nano))
	}
	if request.Resource != "" {
		query += ` AND (e.resource_type = ? OR e.resource_id = ?)`
		args = append(args, request.Resource, request.Resource)
	}
	if request.Kind != "" {
		query += ` AND e.kind = ?`
		args = append(args, request.Kind)
	}
	if request.Actor != "" {
		query += ` AND e.actor = ?`
		args = append(args, request.Actor)
	}
	query += ` ORDER BY e.incarnation, e.seq LIMIT ?`
	args = append(args, request.Limit+1)
	rows, err := b.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("serverapi: query canonical audit events: %w", err)
	}
	defer rows.Close()
	events := make([]webui.AuditEvent, 0, request.Limit+1)
	for rows.Next() {
		var storedAgent, occurredAt, kind, resourceType, resourceID, action string
		var incarnation, seq int64
		var actor, storedProject, operationID, projectAgent sql.NullString
		var metadata []byte
		if err := rows.Scan(&storedAgent, &incarnation, &seq, &occurredAt, &kind,
			&actor, &storedProject, &operationID, &resourceType, &resourceID, &action, &metadata, &projectAgent); err != nil {
			clear(metadata)
			return nil, nil, &corruptDataError{boundary: "audit_events row", cause: err}
		}
		event, err := decodeStoredAuditEvent(agentID, projectUID, storedAgent, incarnation, seq, occurredAt, kind,
			actor, storedProject, operationID, resourceType, resourceID, action, projectAgent, metadata)
		clear(metadata)
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("serverapi: iterate canonical audit events: %w", err)
	}
	var next *webui.AuditCursor
	if len(events) > request.Limit {
		events = events[:request.Limit]
		cursor := events[len(events)-1].Cursor
		next = &cursor
	}
	return events, next, nil
}

func decodeStoredAuditEvent(agentID, requestedProject, storedAgent string, incarnation, seq int64, occurredAt, kind string,
	actor, storedProject, operationID sql.NullString, resourceType, resourceID, action string, projectAgent sql.NullString, metadata []byte,
) (webui.AuditEvent, error) {
	boundary := "audit_events canonical event"
	if storedAgent != agentID || incarnation < 1 || seq < 1 || !utf8.ValidString(kind) || kind == "" {
		return webui.AuditEvent{}, &corruptDataError{boundary: boundary, cause: errors.New("invalid indexed identity")}
	}
	envelope, err := auditevents.Decode(metadata)
	if err != nil {
		return webui.AuditEvent{}, &corruptDataError{boundary: boundary, cause: err}
	}
	defer func() {
		for key := range envelope.Event.Attributes {
			delete(envelope.Event.Attributes, key)
		}
	}()
	canonicalTime := envelope.Event.FirstAt.UTC().Format(time.RFC3339Nano)
	if occurredAt != canonicalTime || kind != string(envelope.Event.Kind) ||
		resourceType != envelope.Event.ResourceType || resourceID != envelope.Event.ResourceID || action != envelope.Event.Action ||
		actor.String != envelope.Event.Actor || actor.Valid != (envelope.Event.Actor != "") ||
		storedProject.String != envelope.ProjectUID || storedProject.Valid != (envelope.ProjectUID != "") ||
		operationID.String != envelope.OperationID || operationID.Valid != (envelope.OperationID != "") {
		return webui.AuditEvent{}, &corruptDataError{boundary: boundary, cause: errors.New("indexed fields disagree with canonical payload")}
	}
	if requestedProject != "" && envelope.ProjectUID != requestedProject {
		return webui.AuditEvent{}, &corruptDataError{boundary: boundary, cause: errors.New("event escaped project scope")}
	}
	if projectAgent.Valid && projectAgent.String != agentID {
		return webui.AuditEvent{}, &corruptDataError{boundary: boundary, cause: errors.New("project belongs to another Agent")}
	}
	known := envelope.KnownDurableThrough
	if known != nil {
		copy := *known
		known = &copy
	}
	return webui.AuditEvent{
		Cursor:     webui.AuditCursor{Incarnation: uint64(incarnation), Seq: uint64(seq)},
		OccurredAt: envelope.Event.FirstAt.UTC(), LastAt: envelope.Event.LastAt.UTC(),
		Kind: string(envelope.Event.Kind), ResourceType: envelope.Event.ResourceType,
		ResourceID: envelope.Event.ResourceID, Action: envelope.Event.Action, Actor: envelope.Event.Actor,
		ProjectUID: envelope.ProjectUID, OperationID: envelope.OperationID, Count: envelope.Event.Count,
		PreviousIncarnation: envelope.PreviousIncarnation, KnownDurableThrough: known, ContinuityReason: envelope.Reason,
	}, nil
}

func (b *Backend) loadAuditCoverage(ctx context.Context, agentID string) (webui.AuditCoverage, error) {
	coverage := webui.AuditCoverage{Gaps: make([]webui.AuditCoverageGap, 0), UnknownIncarnations: make([]uint64, 0)}
	start, reason, found, err := b.audit.CoverageStart(ctx, b.auditArchiveID, agentID)
	if err != nil {
		if errors.Is(err, auditstore.ErrInvariant) {
			return webui.AuditCoverage{}, &corruptDataError{boundary: "audit coverage start", cause: err}
		}
		return webui.AuditCoverage{}, fmt.Errorf("serverapi: load audit coverage start: %w", err)
	}
	if !found {
		return coverage, nil
	}
	coverage.Established = true
	coverage.Start = &webui.AuditCoverageStart{
		Cursor: webui.AuditCursor{Incarnation: start.Incarnation, Seq: start.Seq}, Reason: string(reason),
	}
	var currentRevision int64
	if err := b.store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(MAX(coverage_revision), 0)
		FROM agent_coverage_claims WHERE agent_id = ?
	`, agentID).Scan(&currentRevision); err != nil {
		return webui.AuditCoverage{}, fmt.Errorf("serverapi: load current audit coverage revision: %w", err)
	}
	if currentRevision < 0 {
		return webui.AuditCoverage{}, &corruptDataError{boundary: "agent_coverage_claims", cause: errors.New("negative coverage revision")}
	}
	online := false
	if session, ok := b.registry.Current(agentID); ok {
		online = session.State() == producttransport.StateActive
	}
	observation, err := b.audit.Observe(ctx, b.auditArchiveID, agentID, online, uint64(currentRevision), time.Now().UTC())
	if err != nil {
		if errors.Is(err, auditstore.ErrInvariant) {
			return webui.AuditCoverage{}, &corruptDataError{boundary: "audit observation", cause: err}
		}
		return webui.AuditCoverage{}, fmt.Errorf("serverapi: observe audit coverage: %w", err)
	}
	coverage.ACK = auditCursorFromStore(observation.ACKCursor)
	coverage.CoverageRevisionSeen = observation.CoverageRevisionSeen
	coverage.CoverageRevisionCurrent = observation.CoverageRevisionCurrent
	coverage.ACKWatermarkStalledSeconds = int64(observation.ACKWatermarkStalled / time.Second)
	coverage.ACKBlockedWhileIngesting = observation.ACKBlockedWhileIngesting
	coverage.ACKBlockedWhileIngestingFor = int64(observation.ACKBlockedWhileIngestingFor / time.Second)
	coverage.IngestedUnackedRecords = observation.IngestedUnackedRecords
	coverage.EffectiveGapRecords = observation.EffectiveGapRecords
	coverage.AgentGapClaimsTotal = observation.AgentGapClaimsTotal
	if err := b.loadDeliveryNext(ctx, agentID, &coverage); err != nil {
		return webui.AuditCoverage{}, err
	}
	if err := b.loadEffectiveCoverageEntries(ctx, agentID, &coverage); err != nil {
		return webui.AuditCoverage{}, err
	}
	return coverage, nil
}

func auditCursorFromStore(cursor *auditstore.Cursor) *webui.AuditCursor {
	if cursor == nil {
		return nil
	}
	return &webui.AuditCursor{Incarnation: cursor.Incarnation, Seq: cursor.Seq}
}

func (b *Backend) loadDeliveryNext(ctx context.Context, agentID string, coverage *webui.AuditCoverage) error {
	var incarnation, seq sql.NullInt64
	if err := b.store.DB().QueryRowContext(ctx, `
		SELECT next_incarnation, next_seq FROM agent_cursors
		WHERE audit_archive_id = ? AND agent_id = ?
	`, b.auditArchiveID, agentID).Scan(&incarnation, &seq); err != nil {
		return fmt.Errorf("serverapi: load audit delivery cursor: %w", err)
	}
	if incarnation.Valid != seq.Valid || incarnation.Valid && (incarnation.Int64 < 1 || seq.Int64 < 1) {
		return &corruptDataError{boundary: "agent_cursors delivery cursor", cause: errors.New("invalid cursor")}
	}
	if incarnation.Valid {
		coverage.DeliveryNext = &webui.AuditCursor{Incarnation: uint64(incarnation.Int64), Seq: uint64(seq.Int64)}
	}
	return nil
}

func (b *Backend) loadEffectiveCoverageEntries(ctx context.Context, agentID string, coverage *webui.AuditCoverage) error {
	rows, err := b.store.DB().QueryContext(ctx, `
		SELECT entry_type, from_incarnation, from_seq, until_incarnation, until_seq,
		       precision, source, reason, established_at
		FROM server_archive_coverage
		WHERE audit_archive_id = ? AND agent_id = ?
		  AND entry_type IN ('GAP', 'REGRESSION') AND effective = 1 AND resolved_at IS NULL
		ORDER BY from_incarnation, COALESCE(from_seq, 0), id
		LIMIT ?
	`, b.auditArchiveID, agentID, maxAuditCoverageEntries+1)
	if err != nil {
		return fmt.Errorf("serverapi: query effective audit coverage: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var entryType string
		var fromInc int64
		var fromSeq, untilInc, untilSeq sql.NullInt64
		var precision, source, established string
		var reason sql.NullString
		if err := rows.Scan(&entryType, &fromInc, &fromSeq, &untilInc, &untilSeq, &precision, &source, &reason, &established); err != nil {
			return &corruptDataError{boundary: "server_archive_coverage row", cause: err}
		}
		if err := validateCoverageEntry(entryType, fromInc, fromSeq, untilInc, untilSeq, precision, source, reason); err != nil {
			return err
		}
		establishedAt, err := time.Parse(time.RFC3339Nano, established)
		if err != nil || establishedAt.UTC().Format(time.RFC3339Nano) != established {
			return &corruptDataError{boundary: "server_archive_coverage established_at", cause: errors.New("invalid canonical timestamp")}
		}
		count++
		if count > maxAuditCoverageEntries {
			coverage.CoverageEntriesTruncated = true
			continue
		}
		if !fromSeq.Valid {
			coverage.UnknownIncarnations = append(coverage.UnknownIncarnations, uint64(fromInc))
			continue
		}
		coverage.Gaps = append(coverage.Gaps, webui.AuditCoverageGap{
			Type:      entryType,
			From:      webui.AuditCursor{Incarnation: uint64(fromInc), Seq: uint64(fromSeq.Int64)},
			Until:     webui.AuditCursor{Incarnation: uint64(untilInc.Int64), Seq: uint64(untilSeq.Int64)},
			Precision: precision, Source: source, Reason: reason.String, EstablishedAt: establishedAt.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("serverapi: iterate effective audit coverage: %w", err)
	}
	return nil
}

func validateCoverageEntry(entryType string, fromInc int64, fromSeq, untilInc, untilSeq sql.NullInt64, precision, source string, reason sql.NullString) error {
	invalid := func(message string) error {
		return &corruptDataError{boundary: "server_archive_coverage", cause: errors.New(message)}
	}
	if fromInc < 1 || !utf8.ValidString(source) ||
		(precision != "exact" && precision != "coalesced" && precision != "unknown") {
		return invalid("invalid coverage identity")
	}
	switch entryType {
	case "GAP":
		if source != "AGENT_GAP" && source != "AGENT_CONTINUITY_UNCERTAIN" && source != "SERVER_RETENTION" {
			return invalid("invalid gap source")
		}
	case "REGRESSION":
		if source != "SERVER_CURSOR_REGRESSION" {
			return invalid("invalid regression source")
		}
	default:
		return invalid("invalid coverage entry type")
	}
	if !fromSeq.Valid {
		if untilInc.Valid || untilSeq.Valid || precision != "unknown" || reason.Valid ||
			(source != "AGENT_GAP" && source != "AGENT_CONTINUITY_UNCERTAIN") {
			return invalid("invalid unknown-incarnation coverage")
		}
		return nil
	}
	if !untilInc.Valid || !untilSeq.Valid || fromSeq.Int64 < 1 || untilInc.Int64 < 1 || untilSeq.Int64 < 1 ||
		untilInc.Int64 < fromInc || untilInc.Int64 == fromInc && untilSeq.Int64 <= fromSeq.Int64 {
		return invalid("invalid coverage range")
	}
	switch source {
	case "AGENT_GAP", "AGENT_CONTINUITY_UNCERTAIN":
		if reason.Valid {
			return invalid("Agent coverage reason was invented")
		}
	case "SERVER_RETENTION":
		if !reason.Valid || (reason.String != "SERVER_RETENTION_APPLIED" && reason.String != "QUOTA_PRESSURE_BEFORE_AGENT_ACK") {
			return invalid("invalid Server retention reason")
		}
	case "SERVER_CURSOR_REGRESSION":
		if !reason.Valid || (reason.String != "DATABASE_RESTORE" && reason.String != "ARCHIVE_ROLLBACK" &&
			reason.String != "CURSOR_METADATA_LOSS" && reason.String != "UNKNOWN") {
			return invalid("invalid Server cursor regression reason")
		}
	}
	return nil
}
