package auditsync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/east-true/dockpilot/internal/auditstore"
	"github.com/east-true/dockpilot/internal/producttransport"
)

type EventDecoder interface {
	Decode(context.Context, producttransport.SessionInfo, producttransport.AuditRecord) (auditstore.Event, error)
}

type EventDecoderFunc func(context.Context, producttransport.SessionInfo, producttransport.AuditRecord) (auditstore.Event, error)

func (f EventDecoderFunc) Decode(ctx context.Context, info producttransport.SessionInfo, record producttransport.AuditRecord) (auditstore.Event, error) {
	return f(ctx, info, record)
}

type ServerConfig struct {
	Store               *auditstore.Store
	ArchiveID           string
	CoverageStartReason auditstore.CoverageStartReason
	Decoder             EventDecoder
	Now                 func() time.Time
	// ServerIdentityID and ArchiveGeneration are announced to the Agent at the
	// start of every stream so it can apply the archive judgement of
	// architecture 6.4. They are required at protocol N.
	ServerIdentityID  string
	ArchiveGeneration uint64
}

type Server struct{ config ServerConfig }

func NewServer(config ServerConfig) (*Server, error) {
	if config.Store == nil || config.ArchiveID == "" || config.Decoder == nil {
		return nil, errors.New("auditsync: Server store, archive ID, and decoder are required")
	}
	if config.ServerIdentityID == "" || config.ArchiveGeneration == 0 {
		return nil, errors.New("auditsync: Server identity and archive generation are required to announce the archive")
	}
	if config.CoverageStartReason == "" {
		config.CoverageStartReason = auditstore.CoverageServerNeverHad
	}
	if config.CoverageStartReason != auditstore.CoverageServerNeverHad &&
		config.CoverageStartReason != auditstore.CoverageNewAuditArchive &&
		config.CoverageStartReason != auditstore.CoverageDatabaseReinitialized {
		return nil, errors.New("auditsync: invalid coverage-start reason")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Server{config: config}, nil
}

func (s *Server) Run(ctx context.Context, session producttransport.AuditControlSession) error {
	if session == nil {
		return errors.New("auditsync: session is required")
	}
	info := session.Info()
	if info.AgentID == "" {
		return errors.New("auditsync: session Agent identity is empty")
	}
	_, _, established, err := s.config.Store.CoverageStart(ctx, s.config.ArchiveID, info.AgentID)
	if err != nil {
		return err
	}
	stream, err := session.OpenAuditSync(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	// The archive announcement precedes every acknowledgement so the Agent
	// decides whether this is a normal reconnect, a forward Archive Rebind, or a
	// rejection before it streams a single record. A protocol N-1 Agent ignores
	// the message and keeps its existing binding.
	if info.ProtocolVersion >= producttransport.CurrentProductProtocolVersion {
		if err := stream.SendAck(producttransport.AuditAck{
			AuditArchiveID: s.config.ArchiveID,
			Archive: &producttransport.AuditArchiveDescriptor{
				ServerIdentityID: s.config.ServerIdentityID,
				Generation:       s.config.ArchiveGeneration,
				AuditArchiveID:   s.config.ArchiveID,
			},
		}); err != nil {
			return err
		}
	}

	var pending *producttransport.AuditCoverageSnapshot
	var revision uint64
	for {
		message, err := stream.Recv(ctx)
		if err != nil {
			return err
		}
		switch {
		case message.Coverage != nil:
			pending = cloneTransportCoverage(message.Coverage)
			if !established {
				if start, ok := coverageMinimum(*pending); ok {
					if err := s.establish(ctx, info.AgentID, start); err != nil {
						return err
					}
					established = true
				}
			}
			if established {
				revision, err = s.applyCoverage(ctx, info.AgentID, *pending)
				if err != nil {
					return err
				}
				pending = nil
			}
		case message.CursorBehindFloor != nil:
			behind := message.CursorBehindFloor
			start := behind.Requested
			if floor := behind.Bounds.WALFloor; floor != nil && compareTransportCursor(*floor, start) < 0 {
				start = *floor
			}
			if minimum, ok := coverageMinimum(behind.Coverage); ok && compareTransportCursor(minimum, start) < 0 {
				start = minimum
			}
			if !established {
				if err := s.establish(ctx, info.AgentID, start); err != nil {
					return err
				}
				established = true
			}
			revision, err = s.applyCoverage(ctx, info.AgentID, behind.Coverage)
			if err != nil {
				return err
			}
			pending = nil
		case message.Record != nil:
			record := *message.Record
			if !established {
				start := producttransport.AuditCursor{Incarnation: record.Incarnation, Sequence: record.Sequence}
				if pending != nil {
					if minimum, ok := coverageMinimum(*pending); ok && compareTransportCursor(minimum, start) < 0 {
						start = minimum
					}
				}
				if err := s.establish(ctx, info.AgentID, start); err != nil {
					return err
				}
				established = true
			}
			if pending != nil {
				revision, err = s.applyCoverage(ctx, info.AgentID, *pending)
				if err != nil {
					return err
				}
				pending = nil
			}
			if err := s.ingestAndACK(ctx, info, stream, record, &revision); err != nil {
				return err
			}
		default:
			return errors.New("auditsync: unexpected Agent message")
		}
	}
}

func (s *Server) ingestAndACK(ctx context.Context, info producttransport.SessionInfo, stream producttransport.AuditReceiveStream, record producttransport.AuditRecord, revision *uint64) error {
	event, err := s.config.Decoder.Decode(ctx, info, record)
	if err != nil {
		return fmt.Errorf("auditsync: decode event: %w", err)
	}
	if event.AgentID != info.AgentID || event.Cursor != (auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence}) {
		return errors.New("auditsync: decoder changed record identity")
	}
	if record.Sequence == math.MaxUint64 {
		return errors.New("auditsync: Audit sequence exhausted")
	}
	next := auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence + 1}
	if _, err := s.config.Store.Ingest(ctx, s.config.ArchiveID, info.AgentID, []auditstore.Event{event}, next, s.now()); err != nil {
		return err
	}
	proposed := auditstore.Cursor{Incarnation: record.Incarnation, Seq: record.Sequence}
	for {
		if err := stream.SendAck(producttransport.AuditAck{AuditArchiveID: s.config.ArchiveID,
			Incarnation: proposed.Incarnation, Sequence: proposed.Seq, CoverageRevisionSeen: *revision}); err != nil {
			return err
		}
		response, err := stream.Recv(ctx)
		if err != nil {
			return err
		}
		if response.AckResult == nil || response.AckResult.Proposed != (producttransport.AuditCursor{
			Incarnation: proposed.Incarnation, Sequence: proposed.Seq,
		}) {
			return errors.New("auditsync: missing or mismatched ACK result")
		}
		if response.AckResult.Accepted {
			_, err := s.config.Store.CheckAndAdvanceACK(ctx, s.config.ArchiveID, info.AgentID, proposed, *revision, s.now())
			return err
		}
		if response.AckResult.Error != "STALE_COVERAGE" || response.AckResult.StaleCoverage == nil {
			return fmt.Errorf("auditsync: Agent rejected ACK: %s", response.AckResult.Error)
		}
		*revision, err = s.applyCoverage(ctx, info.AgentID, *response.AckResult.StaleCoverage)
		if err != nil {
			return err
		}
	}
}

func (s *Server) establish(ctx context.Context, agentID string, start producttransport.AuditCursor) error {
	return s.config.Store.EstablishCoverageStart(ctx, s.config.ArchiveID, agentID,
		auditstore.Cursor{Incarnation: start.Incarnation, Seq: start.Sequence}, s.config.CoverageStartReason, s.now())
}

func (s *Server) applyCoverage(ctx context.Context, agentID string, coverage producttransport.AuditCoverageSnapshot) (uint64, error) {
	snapshot := auditstore.CoverageSnapshot{AgentID: agentID, Revision: coverage.Revision,
		GeneratedAt: coverage.GeneratedAt, CoverageUnknownIncarnations: append([]uint64(nil), coverage.CoverageUnknownIncarnations...)}
	for _, gap := range coverage.Gaps {
		snapshot.Gaps = append(snapshot.Gaps, auditstore.GapClaim{Incarnation: gap.Incarnation,
			FromSeq: gap.FromSequence, UntilSeq: gap.UntilSequence, Reason: gap.Reason, Precision: auditstore.Precision(gap.Precision)})
	}
	result, err := s.config.Store.ApplyCoverageSnapshot(ctx, s.config.ArchiveID, snapshot, s.now())
	if err != nil {
		return 0, err
	}
	return result.CurrentRevision, nil
}

func (s *Server) now() time.Time { return s.config.Now().UTC() }

func coverageMinimum(coverage producttransport.AuditCoverageSnapshot) (producttransport.AuditCursor, bool) {
	var result producttransport.AuditCursor
	set := false
	for _, gap := range coverage.Gaps {
		candidate := producttransport.AuditCursor{Incarnation: gap.Incarnation, Sequence: gap.FromSequence}
		if !set || compareTransportCursor(candidate, result) < 0 {
			result, set = candidate, true
		}
	}
	for _, incarnation := range coverage.CoverageUnknownIncarnations {
		candidate := producttransport.AuditCursor{Incarnation: incarnation, Sequence: 1}
		if !set || compareTransportCursor(candidate, result) < 0 {
			result, set = candidate, true
		}
	}
	return result, set
}

func compareTransportCursor(left, right producttransport.AuditCursor) int {
	if left.Incarnation < right.Incarnation || (left.Incarnation == right.Incarnation && left.Sequence < right.Sequence) {
		return -1
	}
	if left == right {
		return 0
	}
	return 1
}

func cloneTransportCoverage(value *producttransport.AuditCoverageSnapshot) *producttransport.AuditCoverageSnapshot {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Gaps = append([]producttransport.AuditGap(nil), value.Gaps...)
	copy.CoverageUnknownIncarnations = append([]uint64(nil), value.CoverageUnknownIncarnations...)
	return &copy
}
