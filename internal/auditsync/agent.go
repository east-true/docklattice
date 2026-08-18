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

type AgentConfig struct {
	WAL          WAL
	ArchiveID    string
	ReadLimit    int
	PollInterval time.Duration
}

type Agent struct{ config AgentConfig }

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
	return &Agent{config: config}, nil
}

func (a *Agent) SyncAudit(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditSyncStream) error {
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
	if bounds.AcknowledgedArchiveID != "" && bounds.AcknowledgedArchiveID != a.config.ArchiveID {
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
		if ack.AuditArchiveID != a.config.ArchiveID || ack.Incarnation != proposed.Incarnation || ack.Sequence != proposed.Seq {
			return fmt.Errorf("auditsync: ACK does not match proposed cursor/archive")
		}
		err = a.config.WAL.AckAudit(a.config.ArchiveID, proposed, ack.CoverageRevisionSeen)
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
