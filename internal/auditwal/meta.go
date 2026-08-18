package auditwal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

const (
	metadataFile    = "coverage-state.json"
	metadataVersion = 1
)

type metadata struct {
	Version                     int      `json:"version"`
	AgentID                     string   `json:"agent_id"`
	CurrentIncarnation          uint64   `json:"current_incarnation"`
	NextSeq                     uint64   `json:"next_seq"`
	LastAssigned                *Cursor  `json:"last_assigned,omitempty"`
	DurableThrough              *Cursor  `json:"durable_through,omitempty"`
	AcknowledgedArchive         string   `json:"acknowledged_archive_id,omitempty"`
	ServerACK                   *Cursor  `json:"server_acked_through,omitempty"`
	CoverageRevision            uint64   `json:"coverage_revision"`
	Gaps                        []Gap    `json:"gaps,omitempty"`
	CoverageUnknownIncarnations []uint64 `json:"coverage_unknown_incarnations,omitempty"`
}

func loadMetadata(dir, agentID string, incarnation uint64) (metadata, error) {
	meta, exists, err := readMetadata(dir)
	if err != nil {
		return metadata{}, err
	}
	if !exists {
		return metadata{
			Version: metadataVersion, AgentID: agentID,
			CurrentIncarnation: incarnation, NextSeq: 1,
		}, nil
	}
	if meta.Version != metadataVersion || meta.AgentID != agentID {
		return metadata{}, fmt.Errorf("%w: metadata identity/version mismatch", ErrInvariant)
	}
	if meta.CurrentIncarnation > incarnation {
		return metadata{}, fmt.Errorf("%w: persisted incarnation %d exceeds current %d",
			ErrInvariant, meta.CurrentIncarnation, incarnation)
	}
	if meta.CurrentIncarnation < incarnation {
		meta.CurrentIncarnation = incarnation
		meta.NextSeq = 1
	}
	if err := validateMetadata(meta); err != nil {
		return metadata{}, err
	}
	return meta, nil
}

func readMetadata(dir string) (metadata, bool, error) {
	path := filepath.Join(dir, metadataFile)
	file, err := openSecureWALFile(path, syscall.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return metadata{}, false, nil
	}
	if err != nil {
		return metadata{}, false, fmt.Errorf("auditwal: read metadata: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var meta metadata
	if err := decoder.Decode(&meta); err != nil {
		return metadata{}, false, fmt.Errorf("%w: decode metadata: %v", ErrInvariant, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return metadata{}, false, fmt.Errorf("%w: trailing metadata", ErrInvariant)
	}
	if err := validateMetadata(meta); err != nil {
		return metadata{}, false, err
	}
	return meta, true, nil
}

func validateMetadata(meta metadata) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvariant, message) }
	if meta.Version != metadataVersion || meta.AgentID == "" || meta.CurrentIncarnation == 0 || meta.NextSeq == 0 {
		return invalid("invalid metadata identity, incarnation, or next sequence")
	}
	for name, cursor := range map[string]*Cursor{"last assigned": meta.LastAssigned, "durable through": meta.DurableThrough, "server ACK": meta.ServerACK} {
		if cursor != nil && (cursor.Incarnation == 0 || cursor.Seq == 0 || cursor.Incarnation > meta.CurrentIncarnation) {
			return invalid("invalid " + name + " cursor")
		}
	}
	if meta.LastAssigned == nil {
		if meta.DurableThrough != nil || meta.ServerACK != nil || meta.NextSeq != 1 {
			return invalid("cursor state exists without a last-assigned cursor")
		}
	} else {
		if meta.LastAssigned.Incarnation == meta.CurrentIncarnation {
			if meta.LastAssigned.Seq == ^uint64(0) || meta.NextSeq != meta.LastAssigned.Seq+1 {
				return invalid("next sequence does not follow last assigned")
			}
		} else if meta.NextSeq != 1 {
			return invalid("new incarnation must begin at sequence one")
		}
		if (meta.DurableThrough != nil && compareCursor(*meta.DurableThrough, *meta.LastAssigned) > 0) ||
			(meta.ServerACK != nil && compareCursor(*meta.ServerACK, *meta.LastAssigned) > 0) {
			return invalid("durable or ACK cursor exceeds last assigned")
		}
	}
	if meta.ServerACK != nil && meta.AcknowledgedArchive == "" {
		return invalid("server ACK exists without acknowledged archive")
	}
	if meta.CoverageRevision == 0 && (len(meta.Gaps) != 0 || len(meta.CoverageUnknownIncarnations) != 0) {
		return invalid("coverage loss exists at revision zero")
	}
	unknown := append([]uint64(nil), meta.CoverageUnknownIncarnations...)
	if !sort.SliceIsSorted(unknown, func(i, j int) bool { return unknown[i] < unknown[j] }) {
		return invalid("coverage-unknown incarnations are not sorted")
	}
	for index, incarnation := range unknown {
		if incarnation == 0 || incarnation > meta.CurrentIncarnation || (index > 0 && unknown[index-1] == incarnation) {
			return invalid("invalid coverage-unknown incarnation")
		}
	}
	var previous *Gap
	for index := range meta.Gaps {
		gap := &meta.Gaps[index]
		if gap.Incarnation == 0 || gap.Incarnation > meta.CurrentIncarnation || gap.FromSeq == 0 || gap.UntilSeq <= gap.FromSeq ||
			(gap.Reason != GapRetention && gap.Reason != GapDiskPressure) || (gap.Precision != PrecisionExact && gap.Precision != PrecisionCoalesced) ||
			gap.LastLossRevision == 0 || gap.LastLossRevision > meta.CoverageRevision {
			return invalid("invalid coverage gap")
		}
		if previous != nil && (gap.Incarnation < previous.Incarnation ||
			(gap.Incarnation == previous.Incarnation && gap.FromSeq < previous.UntilSeq)) {
			return invalid("coverage gaps overlap or are unsorted")
		}
		if previous != nil && gap.Incarnation == previous.Incarnation && gap.FromSeq == previous.UntilSeq &&
			gap.Reason == previous.Reason && gap.Precision == previous.Precision {
			return invalid("adjacent equivalent coverage gaps are not canonicalized")
		}
		if sort.Search(len(unknown), func(i int) bool { return unknown[i] >= gap.Incarnation }) < len(unknown) {
			position := sort.Search(len(unknown), func(i int) bool { return unknown[i] >= gap.Incarnation })
			if unknown[position] == gap.Incarnation {
				return invalid("gap duplicates a coverage-unknown incarnation")
			}
		}
		previous = gap
	}
	return nil
}

func saveMetadata(dir string, meta metadata) error {
	if err := validateMetadata(meta); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dir, ".coverage-state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if err := writeFull(temp, data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(dir, metadataFile)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
