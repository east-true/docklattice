package auditsync

import (
	"context"
	"encoding/json"

	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditstore"
	"github.com/east-true/docklattice/internal/producttransport"
)

// CanonicalEventDecoder validates the versioned Agent WAL payload and maps
// only explicit fields into the Server's indexed canonical record. The full
// bounded payload remains metadata; no actor/project/operation is inferred.
type CanonicalEventDecoder struct{}

func (CanonicalEventDecoder) Decode(_ context.Context, info producttransport.SessionInfo, record producttransport.AuditRecord) (auditstore.Event, error) {
	envelope, err := auditevents.Decode(record.Payload)
	if err != nil {
		return auditstore.Event{}, err
	}
	return auditstore.Event{
		AgentID: info.AgentID, Cursor: auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence},
		OccurredAt: envelope.Event.FirstAt, Kind: string(envelope.Event.Kind), Actor: envelope.Event.Actor,
		ProjectUID: envelope.ProjectUID, OperationID: envelope.OperationID,
		ResourceType: envelope.Event.ResourceType, ResourceID: envelope.Event.ResourceID, Action: envelope.Event.Action,
		Metadata: json.RawMessage(append([]byte(nil), record.Payload...)),
	}, nil
}

var _ EventDecoder = CanonicalEventDecoder{}
