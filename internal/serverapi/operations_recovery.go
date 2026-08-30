package serverapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/webui"
)

type recoveredOperation struct {
	spec      operationSpec
	operation webui.Operation
}

func (b *Backend) reconcileAgentOperations(ctx context.Context, agentID string, session producttransport.OperationRecoverySession) error {
	response, err := session.ListActiveOperations(ctx, producttransport.ListActiveOperationsRequest{})
	defer clearActiveOperationTails(response.Operations)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, producttransport.ErrHandlerUnavailable) || errors.Is(err, producttransport.ErrProtocol) {
			return err
		}
		return &liveUnavailableError{agentID: agentID, action: "active operation recovery", cause: err}
	}
	if len(response.Operations) > producttransport.MaxActiveOperationCount {
		return &corruptDataError{boundary: "Agent active operation recovery", cause: errors.New("operation count exceeds protocol limit")}
	}
	validated := make([]recoveredOperation, len(response.Operations))
	previousID := ""
	for index, active := range response.Operations {
		if !validOperationID(active.OperationID) || active.Type == "" || len(active.Type) > 128 || !utf8.ValidString(active.Type) ||
			len(active.ProjectKey) > 1024 || !utf8.ValidString(active.ProjectKey) || len(active.Target) > 1024 || !utf8.ValidString(active.Target) ||
			index > 0 && active.OperationID <= previousID {
			return &corruptDataError{boundary: "Agent active operation recovery", cause: errors.New("invalid, duplicate, or unordered operation metadata")}
		}
		operation, err := operationFromAgent(active.OperationID, active.Operation)
		clear(response.Operations[index].Operation.OutputTail)
		response.Operations[index].Operation.OutputTail = nil
		if err != nil {
			return err
		}
		validated[index] = recoveredOperation{
			spec: operationSpec{
				ID: active.OperationID, AgentID: agentID, ProjectUID: active.ProjectKey,
				Kind: active.Type, Target: active.Target,
			},
			operation: operation,
		}
		previousID = active.OperationID
	}
	return b.mergeRecoveredOperations(ctx, validated)
}

func (b *Backend) mergeRecoveredOperations(ctx context.Context, recovered []recoveredOperation) (err error) {
	defer func() { err = classifyStoreBusy(err) }()
	if lockErr := b.lockOperationMerge(ctx); lockErr != nil {
		return lockErr
	}
	defer b.unlockOperationMerge()
	tx, err := b.store.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("serverapi: begin active operation recovery: %w", err)
	}
	defer tx.Rollback()
	for _, item := range recovered {
		if item.spec.ProjectUID != "" {
			var projectOwned int
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS(SELECT 1 FROM projects WHERE project_uid = ? AND agent_id = ?)
			`, item.spec.ProjectUID, item.spec.AgentID).Scan(&projectOwned); err != nil {
				return fmt.Errorf("serverapi: validate recovered operation project identity: %w", err)
			}
			if projectOwned == 0 {
				return fmt.Errorf("%w: recovered operation references a missing or cross-Agent project", webui.ErrConflict)
			}
		}
		canonical, storedSpec, found, err := loadStoredOperationTx(ctx, tx, item.spec.ID)
		if err != nil {
			return err
		}
		if found {
			if storedSpec != item.spec {
				return fmt.Errorf("%w: recovered operation specification conflicts with Server history", webui.ErrConflict)
			}
			if canonical.Revision > item.operation.Revision {
				continue
			}
			if canonical.Revision == item.operation.Revision {
				if canonical != item.operation {
					return fmt.Errorf("%w: Agent changed recovered operation without increasing revision", webui.ErrConflict)
				}
				continue
			}
			if err := updateRecoveredOperation(ctx, tx, item); err != nil {
				return err
			}
			continue
		}
		if err := insertRecoveredOperation(ctx, tx, item); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("serverapi: commit active operation recovery: %w", err)
	}
	return nil
}

func clearActiveOperationTails(operations []producttransport.ActiveOperation) {
	for index := range operations {
		clear(operations[index].Operation.OutputTail)
		operations[index].Operation.OutputTail = nil
	}
}

func loadStoredOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (webui.Operation, operationSpec, bool, error) {
	var operation webui.Operation
	var spec operationSpec
	var project sql.NullString
	var revision int64
	var rawSummary string
	var output []byte
	err := tx.QueryRowContext(ctx, `
		SELECT id, agent_id, project_uid, kind, status, phase, revision, summary_json,
		       COALESCE(output_tail, X''), output_truncated
		FROM operations WHERE id = ?
	`, operationID).Scan(&operation.ID, &spec.AgentID, &project, &spec.Kind, &operation.Status, &operation.Phase,
		&revision, &rawSummary, &output, &operation.OutputTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return webui.Operation{}, operationSpec{}, false, nil
	}
	if err != nil {
		return webui.Operation{}, operationSpec{}, false, fmt.Errorf("serverapi: load operation during recovery: %w", err)
	}
	if revision < 0 || !utf8.Valid(output) {
		return webui.Operation{}, operationSpec{}, false, &corruptDataError{boundary: "operations", cause: errors.New("invalid operation row")}
	}
	operation.OutputTail = string(output)
	operation.Revision = uint64(revision)
	spec.ID, spec.ProjectUID = operation.ID, project.String
	var summary operationSummary
	if err := decodeStrictJSON([]byte(rawSummary), &summary); err != nil || summary.Version != 1 {
		return webui.Operation{}, operationSpec{}, false, &corruptDataError{boundary: "operations.summary_json", cause: errors.New("invalid operation summary")}
	}
	spec.Target = summary.Target
	operation.PartialEffectsPossible, operation.Error = summary.PartialEffectsPossible, summary.Error
	return operation, spec, true, nil
}

func insertRecoveredOperation(ctx context.Context, tx *sql.Tx, item recoveredOperation) error {
	rawSummary, err := recoveredOperationSummary(item)
	if err != nil {
		return err
	}
	defer clear(rawSummary)
	project := any(nil)
	if item.spec.ProjectUID != "" {
		project = item.spec.ProjectUID
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operations(
			id, agent_id, project_uid, kind, status, phase, revision, actor,
			requested_at, summary_json, output_tail, output_truncated
		) VALUES(?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?)
	`, item.spec.ID, item.spec.AgentID, project, item.spec.Kind, item.operation.Status, item.operation.Phase,
		item.operation.Revision, string(rawSummary), []byte(item.operation.OutputTail), item.operation.OutputTruncated)
	if err != nil {
		return fmt.Errorf("serverapi: insert recovered operation: %w", err)
	}
	return nil
}

func updateRecoveredOperation(ctx context.Context, tx *sql.Tx, item recoveredOperation) error {
	rawSummary, err := recoveredOperationSummary(item)
	if err != nil {
		return err
	}
	defer clear(rawSummary)
	result, err := tx.ExecContext(ctx, `
		UPDATE operations SET status = ?, phase = ?, revision = ?, summary_json = ?, output_tail = ?, output_truncated = ?
		WHERE id = ? AND agent_id = ? AND COALESCE(project_uid, '') = ? AND kind = ?
		  AND COALESCE(json_extract(summary_json, '$.target'), '') = ? AND revision < ?
	`, item.operation.Status, item.operation.Phase, item.operation.Revision, string(rawSummary), []byte(item.operation.OutputTail),
		item.operation.OutputTruncated, item.spec.ID, item.spec.AgentID, item.spec.ProjectUID, item.spec.Kind,
		item.spec.Target, item.operation.Revision)
	if err != nil {
		return fmt.Errorf("serverapi: update recovered operation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("serverapi: inspect recovered operation update: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: operation changed concurrently during recovery", webui.ErrConflict)
	}
	return nil
}

func recoveredOperationSummary(item recoveredOperation) ([]byte, error) {
	raw, err := json.Marshal(operationSummary{
		Version: 1, Target: item.spec.Target, PartialEffectsPossible: item.operation.PartialEffectsPossible,
		Error: item.operation.Error,
	})
	if err != nil {
		return nil, fmt.Errorf("serverapi: encode recovered operation summary: %w", err)
	}
	return raw, nil
}
