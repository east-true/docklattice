// Package auditsync connects the Agent durable Audit WAL to the product P1
// bidirectional stream and the Server canonical Audit store.
package auditsync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/east-true/dockpilot/internal/auditwal"
	"github.com/east-true/dockpilot/internal/producttransport"
)

const (
	DefaultReadLimit    = 64
	DefaultPollInterval = 250 * time.Millisecond
)

type WAL interface {
	Bounds() (auditwal.Bounds, error)
	GetAuditCoverage() (auditwal.CoverageSnapshot, error)
	ReadAuditFrom(context.Context, auditwal.Cursor, int) (auditwal.ReadResult, error)
	AckAudit(string, auditwal.Cursor, uint64) error
}

// ArchiveBinder applies the archive judgement of architecture 6.4 to the
// descriptor a Server announces at the start of an Audit sync stream. It
// returns the archive ID the Agent must use for this stream, which differs from
// the current one exactly when a forward Archive Rebind was performed. A
// rollback, an identity mismatch, or a same-generation archive change is
// reported as an error and ends the stream instead of rebinding.
type ArchiveBinder interface {
	BindArchive(context.Context, producttransport.AuditArchiveDescriptor) (string, error)
}

type ArchiveBinderFunc func(context.Context, producttransport.AuditArchiveDescriptor) (string, error)

func (f ArchiveBinderFunc) BindArchive(ctx context.Context, descriptor producttransport.AuditArchiveDescriptor) (string, error) {
	return f(ctx, descriptor)
}

type AgentConfig struct {
	WAL          WAL
	ArchiveID    string
	ReadLimit    int
	PollInterval time.Duration
	// Binder is consulted for the archive descriptor a protocol N Server
	// announces. Without one the Agent keeps its configured archive, which is
	// the protocol N-1 behaviour.
	Binder ArchiveBinder
}

type Agent struct {
	config    AgentConfig
	archiveID string
}

func NewAgent(config AgentConfig) (*Agent, error) {
	if config.WAL == nil || config.ArchiveID == "" {
		return nil, errors.New("auditsync: WAL and archive ID are required")
	}
	if config.ReadLimit == 0 {
		config.ReadLimit = DefaultReadLimit
	}
	if config.PollInterval == 0 {
		config.PollInterval = DefaultPollInterval
	}
	if config.ReadLimit < 1 || config.PollInterval <= 0 {
		return nil, errors.New("auditsync: read limit and poll interval must be positive")
	}
	return &Agent{config: config, archiveID: config.ArchiveID}, nil
}

func (a *Agent) SyncAudit(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditSyncStream) error {
	// A protocol N Server announces its archive before anything else, so the
	// rebind judgement happens before a single record leaves the WAL. Reading it
	// here is safe because the announcement is unconditional at that version.
	a.archiveID = a.config.ArchiveID
	if info.ProtocolVersion >= producttransport.CurrentProductProtocolVersion {
		announcement, err := stream.ReceiveAck()
		if err != nil {
			return err
		}
		if !announcement.IsArchiveAnnouncement() {
			return errors.New("auditsync: Server did not announce its Audit Archive before acknowledging")
		}
		if a.config.Binder != nil {
			bound, err := a.config.Binder.BindArchive(ctx, *announcement.Archive)
			if err != nil {
				return err
			}
			if bound == "" {
				return errors.New("auditsync: archive binder returned no archive")
			}
			a.archiveID = bound
		}
	}
	coverage, err := a.config.WAL.GetAuditCoverage()
	if err != nil {
		return err
	}
	if coverage.AgentID != info.AgentID {
		return errors.New("auditsync: session and WAL Agent identities differ")
	}
	if err := stream.Send(producttransport.AuditUpstream{Coverage: coverageToTransport(coverage)}); err != nil {
		return err
	}
	lastCoverageRevision := coverage.Revision
	start, err := a.startCursor()
	if err != nil {
		return err
	}

	for {
		coverage, err = a.config.WAL.GetAuditCoverage()
		if err != nil {
			return err
		}
		if coverage.Revision != lastCoverageRevision {
			if err := stream.Send(producttransport.AuditUpstream{Coverage: coverageToTransport(coverage)}); err != nil {
				return err
			}
			lastCoverageRevision = coverage.Revision
		}

		result, err := a.config.WAL.ReadAuditFrom(ctx, start, a.config.ReadLimit)
		if err != nil {
			return err
		}
		if result.BehindFloor != nil {
			behind := behindFloorToTransport(*result.BehindFloor)
			if err := stream.Send(producttransport.AuditUpstream{CursorBehindFloor: &behind}); err != nil {
				return err
			}
			lastCoverageRevision = result.BehindFloor.Coverage.Revision
			if result.BehindFloor.Bounds.WALFloor != nil {
				start = *result.BehindFloor.Bounds.WALFloor
			} else {
				start = result.BehindFloor.Bounds.NextCursor
			}
			continue
		}
		if len(result.Records) == 0 {
			if err := wait(ctx, a.config.PollInterval); err != nil {
				return err
			}
			continue
		}
		for _, record := range result.Records {
			if err := stream.Send(producttransport.AuditUpstream{Record: &producttransport.AuditRecord{
				Incarnation: record.Cursor.Incarnation, Sequence: record.Cursor.Seq,
				AppendedAt: record.AppendedAt, Payload: append([]byte(nil), record.Payload...),
			}}); err != nil {
				return err
			}
			if err := a.finishACK(stream, record.Cursor, &lastCoverageRevision); err != nil {
				return err
			}
			if record.Cursor.Seq == math.MaxUint64 {
				return errors.New("auditsync: Audit sequence exhausted")
			}
			start = auditwal.Cursor{Incarnation: record.Cursor.Incarnation, Seq: record.Cursor.Seq + 1}
		}
	}
}

func (a *Agent) startCursor() (auditwal.Cursor, error) {
	bounds, err := a.config.WAL.Bounds()
	if err != nil {
		return auditwal.Cursor{}, err
	}
	if bounds.AcknowledgedArchiveID != "" && bounds.AcknowledgedArchiveID != a.archiveID {
		return auditwal.Cursor{}, errors.New("auditsync: WAL is bound to a different archive")
	}
	if bounds.ServerACKedThrough != nil {
		if bounds.ServerACKedThrough.Seq == math.MaxUint64 {
			return auditwal.Cursor{}, errors.New("auditsync: Audit sequence exhausted")
		}
		return auditwal.Cursor{Incarnation: bounds.ServerACKedThrough.Incarnation, Seq: bounds.ServerACKedThrough.Seq + 1}, nil
	}
	if bounds.WALFloor != nil {
		return *bounds.WALFloor, nil
	}
	return bounds.NextCursor, nil
}

func (a *Agent) finishACK(stream producttransport.AuditSyncStream, proposed auditwal.Cursor, sentRevision *uint64) error {
	for {
		ack, err := stream.ReceiveAck()
		if err != nil {
			return err
		}
		if ack.AuditArchiveID != a.archiveID || ack.Incarnation != proposed.Incarnation || ack.Sequence != proposed.Seq {
			return fmt.Errorf("auditsync: ACK does not match proposed cursor/archive")
		}
		err = a.config.WAL.AckAudit(a.archiveID, proposed, ack.CoverageRevisionSeen)
		if err == nil {
			return stream.Send(producttransport.AuditUpstream{AckResult: &producttransport.AuditAckResult{
				Proposed: producttransport.AuditCursor{Incarnation: proposed.Incarnation, Sequence: proposed.Seq}, Accepted: true,
			}})
		}
		var stale *auditwal.StaleCoverageError
		if !errors.As(err, &stale) {
			return err
		}
		coverage := coverageToTransport(stale.Coverage)
		*sentRevision = stale.Coverage.Revision
		if err := stream.Send(producttransport.AuditUpstream{AckResult: &producttransport.AuditAckResult{
			Proposed:      producttransport.AuditCursor{Incarnation: proposed.Incarnation, Sequence: proposed.Seq},
			StaleCoverage: coverage, Error: "STALE_COVERAGE",
		}}); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func coverageToTransport(snapshot auditwal.CoverageSnapshot) *producttransport.AuditCoverageSnapshot {
	result := &producttransport.AuditCoverageSnapshot{Revision: snapshot.Revision, GeneratedAt: snapshot.GeneratedAt,
		CoverageUnknownIncarnations: append([]uint64(nil), snapshot.CoverageUnknownIncarnations...)}
	for _, gap := range snapshot.Gaps {
		result.Gaps = append(result.Gaps, producttransport.AuditGap{Incarnation: gap.Incarnation,
			FromSequence: gap.FromSeq, UntilSequence: gap.UntilSeq, Reason: string(gap.Reason),
			Precision: string(gap.Precision), LastLossRevision: gap.LastLossRevision})
	}
	return result
}

func behindFloorToTransport(value auditwal.CursorBehindFloor) producttransport.AuditCursorBehindFloor {
	return producttransport.AuditCursorBehindFloor{
		Requested: producttransport.AuditCursor{Incarnation: value.Requested.Incarnation, Sequence: value.Requested.Seq},
		Bounds:    boundsToTransport(value.Bounds), Coverage: *coverageToTransport(value.Coverage),
	}
}

func boundsToTransport(value auditwal.Bounds) producttransport.AuditBounds {
	return producttransport.AuditBounds{WALFloor: optionalCursorToTransport(value.WALFloor),
		WALCeiling: optionalCursorToTransport(value.WALCeiling), NextCursor: producttransport.AuditCursor{
			Incarnation: value.NextCursor.Incarnation, Sequence: value.NextCursor.Seq,
		}, ServerACKedThrough: optionalCursorToTransport(value.ServerACKedThrough),
		AcknowledgedArchiveID: value.AcknowledgedArchiveID, CoverageRevision: value.CoverageRevision}
}

func optionalCursorToTransport(value *auditwal.Cursor) *producttransport.AuditCursor {
	if value == nil {
		return nil
	}
	return &producttransport.AuditCursor{Incarnation: value.Incarnation, Sequence: value.Seq}
}
