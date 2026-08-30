// Package auditevents connects Docker event signals and managed-operation
// completions to the Agent audit WAL using one bounded payload format.
package auditevents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/auditgen"
)

const (
	PayloadVersion                                = 1
	MaxPayloadBytes                               = 32 << 10
	KindContinuityUncertain         auditgen.Kind = "AUDIT_CONTINUITY_UNCERTAIN"
	ContinuityReasonUncleanShutdown               = "UNCLEAN_SHUTDOWN"
	maxResourceType                               = 64
	maxResourceID                                 = 512
	maxAction                                     = 128
	maxActor                                      = 160
	maxProjectUID                                 = 256
	maxOperationID                                = 256
	maxAttributes                                 = 32
	maxAttributeKey                               = 128
	maxAttributeVal                               = 1024
)

var ErrInvalidPayload = errors.New("AUDIT_EVENT_INVALID_PAYLOAD")

type payload struct {
	Version             int               `json:"version"`
	Kind                auditgen.Kind     `json:"kind"`
	ResourceType        string            `json:"resource_type"`
	ResourceID          string            `json:"resource_id,omitempty"`
	Action              string            `json:"action"`
	Actor               string            `json:"actor,omitempty"`
	ProjectUID          string            `json:"project_uid,omitempty"`
	OperationID         string            `json:"operation_id,omitempty"`
	FirstAt             time.Time         `json:"first_at"`
	LastAt              time.Time         `json:"last_at"`
	Count               uint64            `json:"count"`
	Attributes          map[string]string `json:"attributes,omitempty"`
	PreviousIncarnation uint64            `json:"previous_incarnation,omitempty"`
	KnownDurableThrough *uint64           `json:"known_durable_through,omitempty"`
	Reason              string            `json:"reason,omitempty"`
}

// Envelope carries canonical indexing fields alongside the human-readable
// audit event. They are explicit operation facts, never inferred from Docker
// labels or other observed metadata.
type Envelope struct {
	Event               auditgen.Event
	ProjectUID          string
	OperationID         string
	PreviousIncarnation uint64
	KnownDurableThrough *uint64
	Reason              string
}

// Encode returns the versioned, bounded JSON object stored as an auditwal
// payload for observed and EVENT_STORM records. Managed records intentionally
// fail here because their canonical operation_id must be supplied through
// EncodeManaged or EncodeEnvelope.
func Encode(event auditgen.Event) ([]byte, error) {
	return EncodeEnvelope(Envelope{Event: event})
}

// EncodeManaged builds a managed completion payload with the canonical
// Operation lookup fields. operationID is mandatory; projectUID is empty for
// host/container operations that are not attached to a Compose project.
func EncodeManaged(signal auditgen.Signal, actor, projectUID, operationID string) ([]byte, error) {
	event, err := auditgen.Managed(signal, actor)
	if err != nil {
		return nil, err
	}
	return EncodeEnvelope(Envelope{Event: event, ProjectUID: projectUID, OperationID: operationID})
}

// EncodeContinuityUncertain creates the in-band boundary record required
// after an unclean previous incarnation. knownDurableThrough is optional and
// never claims an exact loss count.
func EncodeContinuityUncertain(previousIncarnation uint64, knownDurableThrough *uint64, at time.Time) ([]byte, error) {
	event := auditgen.Event{
		Kind: KindContinuityUncertain, ResourceType: "audit", Action: "continuity_uncertain",
		FirstAt: at.UTC(), LastAt: at.UTC(), Count: 1,
	}
	return EncodeEnvelope(Envelope{
		Event: event, PreviousIncarnation: previousIncarnation,
		KnownDurableThrough: cloneUint64(knownDurableThrough), Reason: ContinuityReasonUncleanShutdown,
	})
}

func EncodeEnvelope(envelope Envelope) ([]byte, error) {
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	event := envelope.Event
	encoded, err := json.Marshal(payload{
		Version: PayloadVersion, Kind: event.Kind, ResourceType: event.ResourceType,
		ResourceID: event.ResourceID, Action: event.Action, Actor: event.Actor,
		ProjectUID: envelope.ProjectUID, OperationID: envelope.OperationID,
		FirstAt: event.FirstAt.UTC(), LastAt: event.LastAt.UTC(), Count: event.Count,
		Attributes:          cloneAttributes(event.Attributes),
		PreviousIncarnation: envelope.PreviousIncarnation,
		KnownDurableThrough: cloneUint64(envelope.KnownDurableThrough), Reason: envelope.Reason,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrInvalidPayload, err)
	}
	if len(encoded) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload exceeds %d bytes", ErrInvalidPayload, MaxPayloadBytes)
	}
	return encoded, nil
}

// Decode strictly validates a WAL payload without depending on Server-side
// storage types. Unknown fields and trailing JSON are rejected.
func Decode(encoded []byte) (Envelope, error) {
	if len(encoded) == 0 || len(encoded) > MaxPayloadBytes {
		return Envelope{}, fmt.Errorf("%w: invalid payload size", ErrInvalidPayload)
	}
	if err := rejectDuplicateKeys(encoded); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value payload
	if err := decoder.Decode(&value); err != nil {
		return Envelope{}, fmt.Errorf("%w: decode: %v", ErrInvalidPayload, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Envelope{}, fmt.Errorf("%w: trailing JSON", ErrInvalidPayload)
	}
	if value.Version != PayloadVersion {
		return Envelope{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidPayload, value.Version)
	}
	event := auditgen.Event{
		Kind: value.Kind, ResourceType: value.ResourceType, ResourceID: value.ResourceID,
		Action: value.Action, Actor: value.Actor, FirstAt: value.FirstAt.UTC(), LastAt: value.LastAt.UTC(),
		Count: value.Count, Attributes: cloneAttributes(value.Attributes),
	}
	envelope := Envelope{
		Event: event, ProjectUID: value.ProjectUID, OperationID: value.OperationID,
		PreviousIncarnation: value.PreviousIncarnation,
		KnownDurableThrough: cloneUint64(value.KnownDurableThrough), Reason: value.Reason,
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func rejectDuplicateKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("invalid object terminator")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("invalid array terminator")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func validateEnvelope(envelope Envelope) error {
	event := envelope.Event
	if !validText(event.ResourceType, maxResourceType, false) || !validText(event.Action, maxAction, false) ||
		!validText(event.Actor, maxActor, true) || event.FirstAt.IsZero() || event.LastAt.IsZero() ||
		event.LastAt.Before(event.FirstAt) || event.Count == 0 ||
		!validText(envelope.ProjectUID, maxProjectUID, true) || !validText(envelope.OperationID, maxOperationID, true) {
		return ErrInvalidPayload
	}
	switch event.Kind {
	case auditgen.KindManaged:
		if !validText(event.ResourceID, maxResourceID, false) || event.Count != 1 || !event.FirstAt.Equal(event.LastAt) || envelope.OperationID == "" || envelope.hasContinuityFields() {
			return ErrInvalidPayload
		}
		_, err := auditgen.Managed(auditgen.Signal{
			ResourceType: event.ResourceType, ResourceID: event.ResourceID, Action: event.Action,
			OccurredAt: event.FirstAt, Attributes: event.Attributes,
		}, event.Actor)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidPayload, err)
		}
	case auditgen.KindObserved:
		if event.Actor != "" || !validText(event.ResourceID, maxResourceID, false) || envelope.ProjectUID != "" || envelope.OperationID != "" || envelope.hasContinuityFields() {
			return ErrInvalidPayload
		}
	case auditgen.KindEventStorm:
		if event.ResourceType != "docker" || event.ResourceID != "" || event.Action != "event_storm" || event.Actor != "" || envelope.ProjectUID != "" || envelope.OperationID != "" || envelope.hasContinuityFields() {
			return ErrInvalidPayload
		}
	case KindContinuityUncertain:
		if event.ResourceType != "audit" || event.ResourceID != "" || event.Action != "continuity_uncertain" ||
			event.Actor != "" || event.Count != 1 || !event.FirstAt.Equal(event.LastAt) || len(event.Attributes) != 0 ||
			envelope.ProjectUID != "" || envelope.OperationID != "" || envelope.PreviousIncarnation == 0 ||
			envelope.Reason != ContinuityReasonUncleanShutdown {
			return ErrInvalidPayload
		}
	default:
		return ErrInvalidPayload
	}
	if len(event.Attributes) > maxAttributes {
		return ErrInvalidPayload
	}
	for key, value := range event.Attributes {
		if !validText(key, maxAttributeKey, false) || !validText(value, maxAttributeVal, true) {
			return ErrInvalidPayload
		}
	}
	return nil
}

func (envelope Envelope) hasContinuityFields() bool {
	return envelope.PreviousIncarnation != 0 || envelope.KnownDurableThrough != nil || envelope.Reason != ""
}

func validText(value string, maximum int, emptyAllowed bool) bool {
	return (emptyAllowed || value != "") && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func cloneAttributes(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
